// Package cache is the Redis layer, with an in-process stand-in so the API
// runs with nothing installed.
//
// What Redis is actually for here, since "it helps when the internet is slow"
// is only half true -- a guest's weak connection is handled on the *client*,
// by react-query's IndexedDB cache and the service worker. Redis earns its
// place on the server side, for four jobs:
//
//  1. Hot reads. The rundown, the guest list and the announcements are read by
//     every phone in the room and change a handful of times all day. Served
//     from Redis they cost about a millisecond instead of a round trip to
//     Supabase, which on a free tier may well sit in another country.
//
//  2. Idempotency. The frontend queues mutations while offline and replays
//     them when the signal comes back, so the same "send question" can arrive
//     twice. Once makes the second arrival return the first one's response
//     instead of creating a duplicate. Without it, offline-first means double
//     questions and double RSVPs.
//
//  3. Stage fan-out. MC and speaker share one live state. In the prototype
//     that is localStorage, which only ever syncs tabs inside one browser.
//     Publish/Subscribe makes it work between two different phones, which is
//     the whole point of the MC and Performance sides.
//
//  4. Rate limiting the invite endpoint, so guest tokens cannot be brute
//     forced by walking the keyspace.
package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrInFlight means an identical request is being processed right now.
var ErrInFlight = errors.New("identical request already in flight")

// pending marks an idempotency key that has been claimed but whose response is
// not stored yet.
const pending = "__pending__"

// Cache is the whole surface the API uses. Keeping it an interface is what lets
// the service run without Redis.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration)
	Delete(ctx context.Context, keys ...string)

	// Once runs produce at most once per key within ttl. The bool reports
	// whether the result came from an earlier call, i.e. a replay.
	Once(ctx context.Context, key string, ttl time.Duration, produce func() ([]byte, error)) ([]byte, bool, error)

	// Allow reports whether this key is still under limit for the window.
	Allow(ctx context.Context, key string, limit int, window time.Duration) bool

	Publish(ctx context.Context, channel string, msg []byte) error

	// Subscribe returns a channel of messages and a stop func that must be
	// called to release the subscription.
	Subscribe(ctx context.Context, channel string) (<-chan []byte, func())

	// Kind is "redis" or "memory", for the boot log and /healthz.
	Kind() string
	Close() error
}

// Open returns a Redis-backed cache, or the in-process one when url is empty.
//
// A bad URL is a hard error rather than a fallback: degrading to memory would
// look fine in the logs and then quietly lose the stage fan-out between
// devices, which is the failure hardest to spot during a live event.
func Open(ctx context.Context, url string) (Cache, error) {
	if url == "" {
		return NewMemory(), nil
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	client := redis.NewClient(opt)

	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(ping).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("connect redis: %w", err)
	}
	return &redisCache{client: client}, nil
}

// ---------- Redis ----------

type redisCache struct{ client *redis.Client }

func (c *redisCache) Kind() string { return "redis" }
func (c *redisCache) Close() error { return c.client.Close() }

func (c *redisCache) Get(ctx context.Context, key string) ([]byte, bool) {
	b, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	return b, true
}

func (c *redisCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) {
	// A failed cache write is not the caller's problem: the next read simply
	// misses and goes to Postgres.
	c.client.Set(ctx, key, val, ttl)
}

func (c *redisCache) Delete(ctx context.Context, keys ...string) {
	if len(keys) > 0 {
		c.client.Del(ctx, keys...)
	}
}

func (c *redisCache) Once(ctx context.Context, key string, ttl time.Duration, produce func() ([]byte, error)) ([]byte, bool, error) {
	if b, ok := c.Get(ctx, key); ok && string(b) != pending {
		return b, true, nil
	}
	// Claim the key. Losing this race means another copy of the same request
	// got there first, so this one must not run produce as well.
	won, err := c.client.SetNX(ctx, key, pending, ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("claim idempotency key: %w", err)
	}
	if !won {
		if b, ok := c.Get(ctx, key); ok && string(b) != pending {
			return b, true, nil
		}
		return nil, false, ErrInFlight
	}

	out, err := produce()
	if err != nil {
		// Release the claim, so retrying a failed write is allowed.
		c.client.Del(ctx, key)
		return nil, false, err
	}
	c.Set(ctx, key, out, ttl)
	return out, false, nil
}

func (c *redisCache) Allow(ctx context.Context, key string, limit int, window time.Duration) bool {
	n, err := c.client.Incr(ctx, key).Result()
	if err != nil {
		// Redis being down must not lock a guest out of their own invitation.
		return true
	}
	if n == 1 {
		c.client.Expire(ctx, key, window)
	}
	return int(n) <= limit
}

func (c *redisCache) Publish(ctx context.Context, channel string, msg []byte) error {
	return c.client.Publish(ctx, channel, msg).Err()
}

func (c *redisCache) Subscribe(ctx context.Context, channel string) (<-chan []byte, func()) {
	sub := c.client.Subscribe(ctx, channel)
	out := make(chan []byte, 8)
	go func() {
		defer close(out)
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-ch:
				if !ok {
					return
				}
				select {
				case out <- []byte(m.Payload):
				default:
					// A subscriber that cannot keep up is skipped rather than
					// stalling the fan-out; an SSE client re-reads the whole
					// state when it reconnects anyway.
				}
			}
		}
	}()
	return out, func() { sub.Close() }
}

// ---------- in-process ----------

type entry struct {
	val     []byte
	expires time.Time
}

type memory struct {
	mu     sync.Mutex
	items  map[string]entry
	subs   map[string]map[chan []byte]struct{}
	closed bool
}

// NewMemory is the no-Redis path. It honours the same contract within one
// process, which is all a single API instance needs. The limit is real though:
// two instances would not see each other's stage updates, and would each keep
// their own rate-limit counters.
func NewMemory() Cache {
	m := &memory{items: map[string]entry{}, subs: map[string]map[chan []byte]struct{}{}}
	go m.reap()
	return m
}

func (m *memory) Kind() string { return "memory" }

func (m *memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, set := range m.subs {
		for ch := range set {
			close(ch)
		}
	}
	m.subs = map[string]map[chan []byte]struct{}{}
	return nil
}

// reap drops expired entries, so a long-running process does not grow by one
// idempotency key per write forever.
func (m *memory) reap() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		now := time.Now()
		for k, e := range m.items {
			if now.After(e.expires) {
				delete(m.items, k)
			}
		}
		m.mu.Unlock()
	}
}

func (m *memory) Get(_ context.Context, key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.items[key]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.val, true
}

func (m *memory) Set(_ context.Context, key string, val []byte, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[key] = entry{val: val, expires: time.Now().Add(ttl)}
}

func (m *memory) Delete(_ context.Context, keys ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.items, k)
	}
}

func (m *memory) Once(ctx context.Context, key string, ttl time.Duration, produce func() ([]byte, error)) ([]byte, bool, error) {
	m.mu.Lock()
	if e, ok := m.items[key]; ok && time.Now().Before(e.expires) {
		m.mu.Unlock()
		if string(e.val) != pending {
			return e.val, true, nil
		}
		return nil, false, ErrInFlight
	}
	m.items[key] = entry{val: []byte(pending), expires: time.Now().Add(ttl)}
	m.mu.Unlock()

	out, err := produce()
	if err != nil {
		m.Delete(ctx, key)
		return nil, false, err
	}
	m.Set(ctx, key, out, ttl)
	return out, false, nil
}

func (m *memory) Allow(_ context.Context, key string, limit int, window time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.items[key]
	if !ok || time.Now().After(e.expires) {
		m.items[key] = entry{val: []byte{1}, expires: time.Now().Add(window)}
		return true
	}
	if len(e.val) == 0 {
		return true
	}
	// Saturating: once the counter tops out the key stays blocked for the rest
	// of the window instead of wrapping around to zero and allowing again.
	if e.val[0] < 255 {
		e.val[0]++
	}
	m.items[key] = e
	return int(e.val[0]) <= limit
}

func (m *memory) Publish(_ context.Context, channel string, msg []byte) error {
	m.mu.Lock()
	targets := make([]chan []byte, 0, len(m.subs[channel]))
	for ch := range m.subs[channel] {
		targets = append(targets, ch)
	}
	m.mu.Unlock()

	for _, ch := range targets {
		select {
		case ch <- msg:
		default: // slow subscriber, same policy as Redis above
		}
	}
	return nil
}

func (m *memory) Subscribe(ctx context.Context, channel string) (<-chan []byte, func()) {
	ch := make(chan []byte, 8)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	if m.subs[channel] == nil {
		m.subs[channel] = map[chan []byte]struct{}{}
	}
	m.subs[channel][ch] = struct{}{}
	m.mu.Unlock()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if set, ok := m.subs[channel]; ok {
				if _, live := set[ch]; live {
					delete(set, ch)
					close(ch)
				}
				if len(set) == 0 {
					delete(m.subs, channel)
				}
			}
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ch, stop
}
