package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerCache(t *testing.T) {
	ctx := context.Background()
	c := NewServerCache(time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }
	builds := 0
	build := func(context.Context) (Valkey, error) { builds++; return newMemValkey(), nil }
	// get takes a lease and gives it back at once, as a reconcile that is over.
	get := func(key, version string) Valkey {
		v, release, err := c.Get(ctx, key, version, build)
		if err != nil {
			t.Fatal(err)
		}
		release()
		return v
	}
	closed := func(v Valkey) int { return v.(*memValkey).closed }

	v1 := get("k", "v1")
	if again := get("k", "v1"); again != v1 || builds != 1 {
		t.Errorf("a hit built a new connection (builds = %d)", builds)
	}

	// A new stamp replaces the entry and closes the old connection.
	v2 := get("k", "v2")
	if v2 == v1 || builds != 2 || closed(v1) != 1 {
		t.Errorf("invalidation: builds = %d, closed = %d", builds, closed(v1))
	}

	// A connection that is no longer alive is replaced.
	v2.(*memValkey).dead = true
	v3 := get("k", "v2")
	if v3 == v2 || builds != 3 || closed(v2) != 1 {
		t.Errorf("dead connection: builds = %d, closed = %d", builds, closed(v2))
	}

	// A failed build caches nothing and keeps no stale entry.
	if _, _, err := c.Get(ctx, "bad", "v", func(context.Context) (Valkey, error) { return nil, errServer }); !errors.Is(err, errServer) || c.Len() != 1 {
		t.Errorf("failed build: %v, len = %d", err, c.Len())
	}
	if _, _, err := c.Get(ctx, "k", "v3", func(context.Context) (Valkey, error) { return nil, errServer }); !errors.Is(err, errServer) || c.Len() != 0 {
		t.Errorf("failed rebuild: %v, len = %d", err, c.Len())
	}

	// Removal: the server is gone.
	v4 := get("gone", "v")
	c.Forget("gone")
	c.Forget("gone")
	if c.Len() != 0 || closed(v4) != 1 {
		t.Errorf("Forget: len = %d, closed = %d", c.Len(), closed(v4))
	}

	// An entry that is not used for longer than the idle time is dropped by the
	// next Get, whatever its key.
	v5 := get("old", "v")
	now = now.Add(30 * time.Second)
	get("young", "v")
	now = now.Add(45 * time.Second)
	get("young", "v")
	if c.Len() != 1 || closed(v5) != 1 {
		t.Errorf("idle: len = %d, closed = %d", c.Len(), closed(v5))
	}
}

// A connection that is replaced, forgotten or idle while a reconcile holds a
// lease on it stays open until the lease is released.
func TestServerCacheClosesAfterTheLastLease(t *testing.T) {
	ctx := context.Background()
	c := NewServerCache(time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }
	build := func(context.Context) (Valkey, error) { return newMemValkey(), nil }

	v1, rel1, _ := c.Get(ctx, "k", "v1", build)
	v1again, rel1b, _ := c.Get(ctx, "k", "v1", build)
	if v1again != v1 {
		t.Fatal("a hit built a new connection")
	}
	// Replaced while two leases are out.
	v2, rel2, _ := c.Get(ctx, "k", "v2", build)
	if v2 == v1 || v1.(*memValkey).closed != 0 {
		t.Fatalf("a replaced connection in use was closed (%d)", v1.(*memValkey).closed)
	}
	rel1()
	rel1() // releasing twice counts once
	if v1.(*memValkey).closed != 0 {
		t.Fatal("closed while the second lease is out")
	}
	rel1b()
	if v1.(*memValkey).closed != 1 {
		t.Fatalf("closed = %d after the last release", v1.(*memValkey).closed)
	}

	// Forgotten while leased.
	c.Forget("k")
	if c.Len() != 0 || v2.(*memValkey).closed != 0 {
		t.Fatalf("Forget closed a connection in use (len %d)", c.Len())
	}
	rel2()
	if v2.(*memValkey).closed != 1 {
		t.Fatal("not closed after its last release")
	}

	// A leased entry is never idle; it is once released and left alone.
	v3, rel3, _ := c.Get(ctx, "i", "v", build)
	now = now.Add(time.Hour)
	_, relX, _ := c.Get(ctx, "other", "v", build)
	relX()
	if c.Len() != 2 || v3.(*memValkey).closed != 0 {
		t.Fatalf("an entry in use was swept (len %d)", c.Len())
	}
	rel3()
	now = now.Add(2 * time.Minute)
	c.mu.Lock()
	c.sweep(c.now())
	c.mu.Unlock()
	if c.Len() != 0 || v3.(*memValkey).closed != 1 {
		t.Fatalf("idle entries stayed (len %d)", c.Len())
	}
}

type guardValkey struct {
	*memValkey
	inUse    atomic.Int32
	closedIn atomic.Bool
}

func (g *guardValkey) Close() {
	if g.inUse.Load() > 0 {
		g.closedIn.Store(true)
	}
	g.memValkey.Close()
}

// Many reconciles that get, use, replace and forget connections at once never
// have one closed under them (run with the race detector).
func TestServerCacheNeverClosesAConnectionInUse(t *testing.T) {
	ctx := context.Background()
	c := NewServerCache(time.Millisecond)
	var mu sync.Mutex
	var all []*guardValkey
	build := func(context.Context) (Valkey, error) {
		g := &guardValkey{memValkey: newMemValkey()}
		mu.Lock()
		all = append(all, g)
		mu.Unlock()
		return g, nil
	}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				v, release, err := c.Get(ctx, "k", fmt.Sprintf("v%d", (i+w)%3), build)
				if err != nil {
					t.Error(err)
					return
				}
				g := v.(*guardValkey)
				g.inUse.Add(1)
				if i%17 == 0 {
					c.Forget("k")
				}
				time.Sleep(time.Microsecond)
				g.inUse.Add(-1)
				release()
			}
		}()
	}
	wg.Wait()
	for _, g := range all {
		if g.closedIn.Load() {
			t.Fatal("a connection was closed while a reconcile used it")
		}
	}
	c.Forget("k")
	for _, g := range all {
		g.mu.Lock()
		n := g.closed
		g.mu.Unlock()
		if n != 1 {
			t.Fatalf("a connection was closed %d times", n)
		}
	}
}

// Start closes idle, unleased entries on its interval without a Get, and stops
// when its context is cancelled.
func TestServerCacheSweeper(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := NewServerCache(time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }
	ticks := make(chan time.Time)
	var interval time.Duration
	stopped := make(chan struct{})
	c.tick = func(d time.Duration) (<-chan time.Time, func()) {
		interval = d
		return ticks, func() { close(stopped) }
	}
	if c.NeedLeaderElection() {
		t.Error("the sweeper needs no leader")
	}
	v, release, _ := c.Get(ctx, "k", "v", func(context.Context) (Valkey, error) { return newMemValkey(), nil })
	held, relHeld, _ := c.Get(ctx, "held", "v", func(context.Context) (Valkey, error) { return newMemValkey(), nil })
	release()

	done := make(chan error)
	go func() { done <- c.Start(ctx) }()
	now = now.Add(time.Hour)
	ticks <- now
	ticks <- now // taken only when the sweep of the first tick is over
	if c.Len() != 1 || v.(*memValkey).closed != 1 || held.(*memValkey).closed != 0 {
		t.Errorf("sweep: len = %d, closed = %d, held closed = %d", c.Len(), v.(*memValkey).closed, held.(*memValkey).closed)
	}
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
	<-stopped
	if c.Len() != 0 || held.(*memValkey).closed != 0 {
		t.Errorf("shutdown: len = %d, held closed = %d (a leased connection stays open)", c.Len(), held.(*memValkey).closed)
	}
	relHeld()
	if held.(*memValkey).closed != 1 {
		t.Errorf("held closed = %d after its release", held.(*memValkey).closed)
	}
	if interval != 30*time.Second {
		t.Errorf("interval = %v", interval)
	}
}

func TestNewTicker(t *testing.T) {
	ch, stop := newTicker(time.Millisecond)
	defer stop()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no tick")
	}
}

func TestNilServerCacheCachesNothing(t *testing.T) {
	var c *ServerCache
	var made []*memValkey
	for range 2 {
		_, release, err := c.Get(context.Background(), "k", "v", func(context.Context) (Valkey, error) {
			m := newMemValkey()
			made = append(made, m)
			return m, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if _, _, err := c.Get(context.Background(), "k", "v", func(context.Context) (Valkey, error) { return nil, errServer }); !errors.Is(err, errServer) {
		t.Errorf("err = %v", err)
	}
	c.Forget("k")
	if len(made) != 2 || made[0].closed != 1 || made[1].closed != 1 {
		t.Errorf("made = %d, a release must close the connection", len(made))
	}
}

// A nil cache caches nothing, and has nothing to count.
func TestNilServerCache(t *testing.T) {
	var c *ServerCache
	if c.Len() != 0 {
		t.Errorf("Len of a nil cache = %d", c.Len())
	}
	c.Forget("k")
}
