// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package discover

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"time"

	"github.com/aristanetworks/goarista/atime"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	ticketTimeBucketLen = time.Minute
	timeWindow          = 30 // * ticketTimeBucketLen
	keepTicketConst     = time.Minute * 20
	keepTicketExp       = time.Minute * 20
	maxRadius           = 0xffffffffffffffff
	minRadAverage       = 1024
)

// absTime represents absolute monotonic time in nanoseconds.
type absTime time.Duration

func monotonicTime() absTime {
	return absTime(atime.NanoTime())
}

// timeBucket represents absolute monotonic time in minutes.
// It is used as the index into the per-topic ticket buckets.
type timeBucket int

type ticket struct {
	topics  []Topic
	regTime []absTime // Per-topic local absolute time when the ticket can be used.

	// The serial number that was issued by the server.
	serial uint32
	// Used by registrar, tracks absolute time when the ticket was created.
	issueTime absTime

	// Fields used only by registrants
	node   *Node  // the registrar node that signed this ticket
	refCnt int    // tracks number of topics that will be registered using this ticket
	pong   []byte // encoded pong packet signed by the registrar
}

// ticketRef refers to a single topic in a ticket.
type ticketRef struct {
	t   *ticket
	idx int // index of the topic in t.topics and t.regTime
}

func (ref ticketRef) topic() Topic {
	return ref.t.topics[ref.idx]
}

func (ref ticketRef) topicRegTime() absTime {
	return ref.t.regTime[ref.idx]
}

func pongToTicket(localTime absTime, topics []Topic, node *Node, p *ingressPacket) (*ticket, error) {
	wps := p.data.(*pong).WaitPeriods
	if len(topics) != len(wps) {
		return nil, fmt.Errorf("bad wait period list: got %d values, want %d", len(topics), len(wps))
	}
	if rlpHash(topics) != p.data.(*pong).TopicHash {
		return nil, fmt.Errorf("bad topic hash")
	}
	t := &ticket{
		issueTime: localTime,
		node:      node,
		topics:    topics,
		pong:      p.rawData,
		regTime:   make([]absTime, len(wps)),
	}
	// Convert wait periods to local absolute time.
	for i, wp := range wps {
		t.regTime[i] = localTime + absTime(time.Second*time.Duration(wp))
	}
	return t, nil
}

func ticketToPong(t *ticket, pong *pong) {
	pong.Expiration = uint64(t.issueTime / absTime(time.Second))
	pong.TopicHash = rlpHash(t.topics)
	pong.TicketSerial = t.serial
	pong.WaitPeriods = make([]uint32, len(t.regTime))
	for i, regTime := range t.regTime {
		pong.WaitPeriods[i] = uint32(time.Duration(regTime-t.issueTime) / time.Second)
	}
}

type ticketStore struct {
	// radius detector and target address generator
	// exists for both searched and registered topics
	radius map[Topic]*topicRadius

	// Contains buckets (for each absolute minute) of tickets
	// that can be used in that minute.
	// This is only set if the topic is being registered.
	tickets   map[Topic]topicTickets
	regtopics []Topic
	nodes     map[*Node]*ticket

	lastBucketFetched    timeBucket
	minRadSum            float64
	minRadCnt, minRadius uint64
	nextTicketCached     *ticketRef
	nextTicketReg        absTime

	log *logChn
}

type topicTickets map[timeBucket][]ticketRef

func newTicketStore() *ticketStore {
	return &ticketStore{
		radius:  make(map[Topic]*topicRadius),
		tickets: make(map[Topic]topicTickets),
		nodes:   make(map[*Node]*ticket),
	}
}

// addTopic starts tracking a topic. If register is true,
// the local node will register the topic and tickets will be collected.
// It can be called even
func (s *ticketStore) addTopic(t Topic, register bool) {
	s.log.log(fmt.Sprintf(" addTopic(%v, %v)", t, register))
	if s.radius[t] == nil {
		s.radius[t] = newTopicRadius(t)
	}
	if register && s.tickets[t] == nil {
		s.tickets[t] = make(topicTickets)
	}
}

// removeRegisterTopic deletes all tickets for the given topic.
func (s *ticketStore) removeRegisterTopic(topic Topic) {
	s.log.log(fmt.Sprintf(" removeRegisterTopic(%v)", topic))
	for _, list := range s.tickets[topic] {
		for _, ref := range list {
			ref.t.refCnt--
			if ref.t.refCnt == 0 {
				delete(s.nodes, ref.t.node)
			}
		}
	}
	delete(s.tickets, topic)
}

func (s *ticketStore) regTopicSet() []Topic {
	topics := make([]Topic, 0, len(s.tickets))
	for topic := range s.tickets {
		topics = append(topics, topic)
	}
	return topics
}

// nextRegisterLookup returns the target of the next lookup for ticket collection.
func (s *ticketStore) nextRegisterLookup() (target common.Hash, delay time.Duration) {
	s.log.log("nextRegisterLookup()")
	firstTopic, ok := s.iterRegTopics()
	for topic := firstTopic; ok; {
		s.log.log(fmt.Sprintf(" checking topic %v, len(s.tickets[topic]) = %d", topic, len(s.tickets[topic])))
		if s.tickets[topic] != nil && s.ticketsInWindow(topic) < 10 {
			next := s.radius[topic].nextTarget()
			s.log.log(fmt.Sprintf(" %x 1s", next[:8]))
			return next, 1 * time.Second
		}
		topic, ok = s.iterRegTopics()
		if topic == firstTopic {
			break // We have checked all topics.
		}
	}
	s.log.log(" null, 40s")
	return common.Hash{}, 40 * time.Second
}

// iterRegTopics returns topics to register in arbitrary order.
// The second return value is false if there are no topics.
func (s *ticketStore) iterRegTopics() (Topic, bool) {
	s.log.log("iterRegTopics()")
	if len(s.regtopics) == 0 {
		if len(s.tickets) == 0 {
			s.log.log(" false")
			return "", false
		}
		// Refill register list.
		for t := range s.tickets {
			s.regtopics = append(s.regtopics, t)
		}
	}
	topic := s.regtopics[len(s.regtopics)-1]
	s.regtopics = s.regtopics[:len(s.regtopics)-1]
	s.log.log(" " + string(topic) + " true")
	return topic, true
}

// ticketsInWindow returns the number of tickets in the registration window.
func (s *ticketStore) ticketsInWindow(t Topic) int {
	now := monotonicTime()
	ltBucket := timeBucket(now / absTime(ticketTimeBucketLen))
	sum := 0
	tickets := s.tickets[t]
	for g := ltBucket; g < ltBucket+timeWindow; g++ {
		sum += len(tickets[g])
	}
	s.log.log(fmt.Sprintf("ticketsInWindow(%v) = %v", t, sum))
	return sum
}

// nextRegisterableTicket returns the next ticket that can be used
// to register.
//
// If the returned wait time <= zero the ticket can be used. For a positive
// wait time, the caller should requery the next ticket later.
//
// A ticket can be returned more than once with <= zero wait time in case
// the ticket contains multiple topics.
func (s *ticketStore) nextRegisterableTicket() (t *ticketRef, wait time.Duration) {
	defer func() {
		if t == nil {
			s.log.log(" nil")
		} else {
			s.log.log(fmt.Sprintf(" node = %x sn = %v wait = %v", t.t.node.ID[:8], t.t.serial, wait))
		}
	}()

	s.log.log("nextRegisterableTicket()")
	now := monotonicTime()
	if s.nextTicketCached != nil {
		return s.nextTicketCached, time.Duration(s.nextTicketCached.topicRegTime() - now)
	}

	for bucket := s.lastBucketFetched; ; bucket++ {
		var (
			empty      = true    // true if there are no tickets
			nextTicket ticketRef // uninitialized if this bucket is empty
		)
		for _, tickets := range s.tickets {
			if len(tickets) != 0 {
				empty = false
				if list := tickets[bucket]; list != nil {
					for _, ref := range list {
						//s.log.log(fmt.Sprintf(" nrt bucket = %d node = %x sn = %v wait = %v", bucket, ref.t.node.ID[:8], ref.t.serial, time.Duration(ref.topicRegTime()-now)))
						if nextTicket.t == nil || ref.topicRegTime() < nextTicket.topicRegTime() {
							nextTicket = ref
						}
					}
				}
			}
		}
		if empty {
			return nil, 0
		}
		if nextTicket.t != nil {
			wait = time.Duration(nextTicket.topicRegTime() - now)
			s.nextTicketCached = &nextTicket
			return &nextTicket, wait
		}
		s.lastBucketFetched = bucket
	}
}

// ticketRegistered is called when t has been used to register for a topic.
func (s *ticketStore) ticketRegistered(ref ticketRef) {
	s.log.log(fmt.Sprintf("ticketRegistered(node = %x sn = %v)", ref.t.node.ID[:8], ref.t.serial))
	bucket := timeBucket(ref.t.issueTime / absTime(ticketTimeBucketLen))
	tickets := s.tickets[ref.topic()]
	list := tickets[bucket]
	for i, bt := range list {
		if bt.t == ref.t {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) != 0 {
		tickets[bucket] = list
	} else {
		delete(tickets, bucket)
	}
	ref.t.refCnt--
	if ref.t.refCnt == 0 {
		delete(s.nodes, ref.t.node)
	}

	// Make nextRegisterableTicket return the next available ticket.
	s.nextTicketCached = nil
}

func (s *ticketStore) registerLookupDone(target common.Hash, nodes []*Node, ping func(n *Node)) {
	now := monotonicTime()
	for _, n := range nodes {
		s.adjustMinRadius(target, n.sha)
		if t := s.nodes[n]; t != nil && now < t.issueTime+absTime(targetWaitTime) {
			// adjust radius with already stored ticket
			s.add(now, t)
		} else {
			// request a new pong packet
			ping(n)
		}
	}
}

func (s *ticketStore) add(localTime absTime, t *ticket) {
	s.log.log(fmt.Sprintf("add(node = %x sn = %v)", t.node.ID[:8], t.serial))
	store := s.nodes[t.node] == nil
	if !store {
		// adjust radius but do not store ticket
		s.log.log(" s.nodes[t.node] != nil")
	}

	bucket := timeBucket(localTime / absTime(ticketTimeBucketLen))
	if s.lastBucketFetched == 0 || bucket < s.lastBucketFetched {
		s.lastBucketFetched = bucket
	}

	for i, topic := range t.topics {
		if tt, ok := s.radius[topic]; ok && tt.isInRadius(t) {
			tt.adjust(localTime, ticketRef{t, i}, s.minRadius)
			s.log.log(fmt.Sprintf("adjust topic: %v, rad: %v, frad: %v, converged: %v", topic, float64(tt.radius)/maxRadius, tt.filteredRadius/maxRadius, tt.converged))

			if store {
				if tickets, ok := s.tickets[topic]; ok && tt.converged {
					wait := t.regTime[i] - localTime
					rnd := rand.ExpFloat64()
					if rnd > 10 {
						rnd = 10
					}
					if float64(wait) < float64(keepTicketConst)+float64(keepTicketExp)*rnd {
						// use the ticket to register this topic
						bucket := timeBucket(t.regTime[i] / absTime(ticketTimeBucketLen))
						tickets[bucket] = append(tickets[bucket], ticketRef{t, i})
						t.refCnt++
					}
				}
			}
		}
	}

	if t.refCnt > 0 {
		s.nextTicketCached = nil
		s.nodes[t.node] = t
	}
}

func (s *ticketStore) getNodeTicket(node *Node) *ticket {
	if s.nodes[node] == nil {
		s.log.log(fmt.Sprintf("getNodeTicket(%x) sn = nil", node.ID[:8]))
	} else {
		s.log.log(fmt.Sprintf("getNodeTicket(%x) sn = %v", node.ID[:8], s.nodes[node].serial))
	}
	return s.nodes[node]
}

func (s *ticketStore) adjustMinRadius(target, found common.Hash) {
	tp := binary.BigEndian.Uint64(target[0:8])
	fp := binary.BigEndian.Uint64(found[0:8])
	dist := tp ^ fp
	mrAdjust := float64(dist) * 16
	if mrAdjust > maxRadius/2 {
		mrAdjust = maxRadius / 2
	}

	if s.minRadCnt < minRadAverage {
		s.minRadCnt++
	} else {
		s.minRadSum -= s.minRadSum / minRadAverage
	}
	s.minRadSum += mrAdjust
	s.minRadius = uint64(s.minRadSum / float64(s.minRadCnt))
	s.log.log(fmt.Sprintf("adjustMinRadius() %v", float64(s.minRadius)/maxRadius))
}

type topicRadius struct {
	topic           Topic
	topicHashPrefix uint64
	radius          uint64
	filteredRadius  float64 // only for convergence detection
	converged       bool
}

const targetWaitTime = time.Minute * 10

func newTopicRadius(t Topic) *topicRadius {
	topicHash := crypto.Keccak256Hash([]byte(t))
	topicHashPrefix := binary.BigEndian.Uint64(topicHash[0:8])

	return &topicRadius{
		topic:           t,
		topicHashPrefix: topicHashPrefix,
		radius:          maxRadius,
		filteredRadius:  float64(maxRadius),
		converged:       false,
	}
}

func (r *topicRadius) isInRadius(t *ticket) bool {
	nodePrefix := binary.BigEndian.Uint64(t.node.sha[0:8])
	dist := nodePrefix ^ r.topicHashPrefix
	return dist < r.radius
}

func (r *topicRadius) nextTarget() common.Hash {
	rnd := uint64(rand.Int63n(int64(r.radius/2))) * 2
	prefix := r.topicHashPrefix ^ rnd
	var target common.Hash
	binary.BigEndian.PutUint64(target[0:8], prefix)
	return target
}

func (r *topicRadius) adjust(localTime absTime, t ticketRef, minRadius uint64) {
	wait := t.t.regTime[t.idx] - localTime
	adjust := (float64(wait)/float64(targetWaitTime) - 1) * 2
	if adjust > 1 {
		adjust = 1
	}
	if adjust < -1 {
		adjust = -1
	}
	if r.converged {
		adjust *= 0.01
	} else {
		adjust *= 0.1
	}

	radius := float64(r.radius) * (1 + adjust)
	if radius > float64(maxRadius) {
		r.radius = maxRadius
		radius = float64(r.radius)
	} else {
		r.radius = uint64(radius)
		if r.radius < minRadius {
			r.radius = minRadius
		}
	}

	if !r.converged {
		if float64(r.radius) >= r.filteredRadius*0.95 {
			r.converged = true
		} else {
			r.filteredRadius += (float64(r.radius) - r.filteredRadius) * 0.05
		}
	}
}

type logChn struct {
	list []string
}

func (c *logChn) log(s string) {
	if c != nil {
		fmt.Println(time.Now().String() + " : " + s)
		//c.list = append(c.list, time.Now().String()+" : "+s)
	}
}

func (c *logChn) printLogs() {
	if c != nil {
		for _, s := range c.list {
			fmt.Println(s)
		}
	}
}

func newlogChn() *logChn {
	return &logChn{}
}
