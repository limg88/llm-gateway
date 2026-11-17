package main

import (
	"math"
	"sync"
	"time"
)

// conversationStore remembers the conversations behind the responses, so
// that a request can continue one with previous_response_id. It lives in
// memory: entries expire after ttl and the oldest go when max is reached.
type conversationStore struct {
	ttl time.Duration
	max int

	mu      sync.Mutex
	entries map[string]*conversation
	order   []string // ids, oldest first
}

type conversation struct {
	owner    string // name of the API key that created it
	messages []ChatMessage
	expires  time.Time
}

func newConversationStore(ttl time.Duration, max int) *conversationStore {
	return &conversationStore{ttl: ttl, max: max, entries: map[string]*conversation{}}
}

func (s *conversationStore) put(id, owner string, messages []ChatMessage) {
	if s.max <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	for len(s.order) >= s.max {
		delete(s.entries, s.order[0])
		s.order = s.order[1:]
	}
	s.entries[id] = &conversation{owner: owner, messages: messages, expires: time.Now().Add(s.ttl)}
	s.order = append(s.order, id)
}

// get returns the conversation of a response, only to the key that owns it.
func (s *conversationStore) get(id, owner string) ([]ChatMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.entries[id]
	if c == nil || c.owner != owner || time.Now().After(c.expires) {
		return nil, false
	}
	return c.messages, true
}

func (s *conversationStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// sweep drops the expired entries. Entries are in creation order and share
// one ttl, so the expired ones are at the front.
func (s *conversationStore) sweep() {
	now := time.Now()
	i := 0
	for ; i < len(s.order); i++ {
		c := s.entries[s.order[i]]
		if c != nil && now.Before(c.expires) {
			break
		}
		delete(s.entries, s.order[i])
	}
	s.order = s.order[i:]
}

// rateLimiter is a token bucket of requests per minute.
type rateLimiter struct {
	mu     sync.Mutex
	rpm    float64
	tokens float64
	last   time.Time
}

func newRateLimiter(rpm int) *rateLimiter {
	return &rateLimiter{rpm: float64(rpm), tokens: float64(rpm), last: time.Now()}
}

// allow takes a token, or tells how long to wait for one.
func (l *rateLimiter) allow() (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens = math.Min(l.rpm, l.tokens+now.Sub(l.last).Minutes()*l.rpm)
	l.last = now
	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}
	return false, time.Duration((1 - l.tokens) / l.rpm * float64(time.Minute))
}
