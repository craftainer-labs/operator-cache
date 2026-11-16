package controller

import (
	"context"
	"sync"
	"time"
)

// ServerCache keeps one logged-in connection per server between reconciles, so
// the operator logs in once instead of for every reconcile. An entry is valid
// for one version, the stamp: the server object (its UID and generation) and
// everything its connection is made from (credentials, CA, proxy), so it is
// replaced as soon as any of them changes, and it is replaced when the
// connection is no longer alive.
//
// Get hands out a lease: the connection and a release function. A replaced or
// forgotten entry is only marked stale; its connection is closed when the last
// lease on it is released, so a reconcile never loses the connection it uses.
// An entry nobody holds a lease on and nobody used for the idle time is closed
// by the next Get or by the sweep that Start runs. A nil *ServerCache caches
// nothing: its leases close the connection on release.
//
// Concurrent Gets for one key run one at a time (a lock per key), so a server's
// connection is built once and the others find it; a Get for another key does
// not wait for that build, and a Get that waits gives up when its context ends.
type ServerCache struct {
	mu      sync.Mutex
	idle    time.Duration
	now     func() time.Time
	tick    func(time.Duration) (<-chan time.Time, func())
	entries map[string]*cacheEntry
	// locks holds the lock of every key a Get is using or waiting for.
	locks map[string]*keyLock
	// closed is set when Start has shut down: nothing is sweeping any more, so a
	// Get no longer caches what it builds.
	closed bool
}

// keyLock is a lock that a waiter can give up on: a channel of one slot, and
// the number of Gets that use it, so the last one removes it from the map.
type keyLock struct {
	slot chan struct{}
	refs int
}

type cacheEntry struct {
	version string
	server  Valkey
	used    time.Time
	leases  int
	stale   bool
}

// NewServerCache returns a cache that closes an unused entry idle for longer than idle.
func NewServerCache(idle time.Duration) *ServerCache {
	return &ServerCache{idle: idle, now: time.Now, tick: newTicker, entries: map[string]*cacheEntry{}, locks: map[string]*keyLock{}}
}

func newTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Get returns a lease on the connection cached for key if it was made for
// version and is alive, else connects through build, replaces the entry and
// retires the old connection. The caller calls the release function once it is
// done with the connection, and not before. A failed build caches nothing.
// build runs with the key's lock held and the cache's lock free: a server that
// does not answer holds up only the Gets for its own key, for the connect
// timeout, and they find its connection once the first has built it. A Forget
// during a build does not stop it: the connection it makes is cached and is
// closed by the idle sweep, or, once Start has shut down, by its release.
func (c *ServerCache) Get(ctx context.Context, key, version string, build func(context.Context) (Valkey, error)) (Valkey, func(), error) {
	if c == nil {
		b, err := build(ctx)
		if err != nil {
			return nil, nil, err
		}
		return b, b.Close, nil
	}
	unlock, err := c.lock(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	c.mu.Lock()
	now := c.now()
	c.sweep(now)
	if e, ok := c.entries[key]; ok && e.version == version && e.server.Alive() {
		defer c.mu.Unlock()
		return c.lease(e, now)
	}
	c.retire(key)
	c.mu.Unlock()
	b, err := build(ctx)
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return b, b.Close, nil
	}
	now = c.now()
	e := &cacheEntry{version: version, server: b, used: now}
	c.entries[key] = e
	return c.lease(e, now)
}

// lock takes the lock of key, or gives up when ctx ends first. The function it
// returns unlocks. The caller does not hold mu.
func (c *ServerCache) lock(ctx context.Context, key string) (func(), error) {
	c.mu.Lock()
	l, ok := c.locks[key]
	if !ok {
		l = &keyLock{slot: make(chan struct{}, 1)}
		c.locks[key] = l
	}
	l.refs++
	c.mu.Unlock()
	done := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if l.refs--; l.refs == 0 {
			delete(c.locks, key)
		}
	}
	select {
	case l.slot <- struct{}{}:
		return func() { <-l.slot; done() }, nil
	case <-ctx.Done():
		done()
		return nil, ctx.Err()
	}
}

// lease counts a new holder of e. The caller holds mu.
func (c *ServerCache) lease(e *cacheEntry, now time.Time) (Valkey, func(), error) {
	e.leases++
	e.used = now
	released := false
	return e.server, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if released {
			return
		}
		released = true
		e.leases--
		e.used = c.now()
		if e.stale && e.leases == 0 {
			e.server.Close()
		}
	}, nil
}

// Forget retires the entry of a server that no longer exists.
func (c *ServerCache) Forget(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retire(key)
}

// Len is the number of cached servers.
func (c *ServerCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// retire removes an entry from the cache. Its connection is closed now when
// nobody holds a lease on it, else by the release of the last lease. The caller
// holds mu.
func (c *ServerCache) retire(key string) {
	e, ok := c.entries[key]
	if !ok {
		return
	}
	delete(c.entries, key)
	e.stale = true
	if e.leases == 0 {
		e.server.Close()
	}
}

// sweep retires the entries nobody holds a lease on and nobody used for longer
// than the idle time. The caller holds mu.
func (c *ServerCache) sweep(now time.Time) {
	for k, e := range c.entries {
		if e.leases == 0 && now.Sub(e.used) > c.idle {
			c.retire(k)
		}
	}
}

// Start implements manager.Runnable: it sweeps the idle entries every
// half idle period, so the connection to a server nobody reconciles any more
// is closed without waiting for a Get. It returns when ctx is cancelled, and
// closes what is left once nobody holds it.
func (c *ServerCache) Start(ctx context.Context) error {
	ticks, stop := c.tick(max(c.idle/2, time.Second))
	defer stop()
	for {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.closed = true
			for k := range c.entries {
				c.retire(k)
			}
			c.mu.Unlock()
			return nil
		case <-ticks:
			c.mu.Lock()
			c.sweep(c.now())
			c.mu.Unlock()
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every replica
// closes its own idle connections.
func (c *ServerCache) NeedLeaderElection() bool { return false }
