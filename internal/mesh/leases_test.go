package mesh

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLeaseFirstClaimerWins(t *testing.T) {
	l := newLeases()
	t0 := time.Unix(1_800_000_000, 0)
	if ok, h := l.claim("k", "a", time.Minute, t0); !ok || h != "a" {
		t.Fatalf("a: %v %q", ok, h)
	}
	if ok, h := l.claim("k", "b", time.Minute, t0.Add(time.Second)); ok || h != "a" {
		t.Fatalf("b while a holds: %v %q", ok, h)
	}
	// The holder renews.
	if ok, _ := l.claim("k", "a", time.Minute, t0.Add(50*time.Second)); !ok {
		t.Fatal("renew refused")
	}
	if ok, _ := l.claim("k", "b", time.Minute, t0.Add(100*time.Second)); ok {
		t.Fatal("renewed lease expired early")
	}
	// After expiry another device wins.
	if ok, h := l.claim("k", "b", time.Minute, t0.Add(111*time.Second)); !ok || h != "b" {
		t.Fatalf("after expiry: %v %q", ok, h)
	}
	if l.holder("k", t0.Add(112*time.Second)) != "b" {
		t.Fatal("holder")
	}
	if l.release("k", "a") {
		t.Fatal("non-holder released")
	}
	if !l.release("k", "b") || l.holder("k", t0) != "" {
		t.Fatal("release")
	}
}

func TestLeaseTTLBounds(t *testing.T) {
	l := newLeases()
	t0 := time.Unix(1_800_000_000, 0)
	l.claim("k", "a", 1000*time.Hour, t0)
	if l.holder("k", t0.Add(maxLeaseTTL+time.Second)) != "" {
		t.Error("ttl not capped")
	}
	l.claim("z", "a", 0, t0)
	if l.holder("z", t0) != "a" || l.holder("z", t0.Add(2*time.Second)) != "" {
		t.Error("zero ttl should be one second")
	}
}

func TestLeasePrune(t *testing.T) {
	l := newLeases()
	t0 := time.Unix(1_800_000_000, 0)
	for i := 0; i < 10; i++ {
		l.claim(fmt.Sprint(i), "a", time.Duration(i+1)*time.Second, t0)
	}
	l.prune(t0.Add(5500 * time.Millisecond))
	if n := l.len(); n != 5 {
		t.Fatalf("after prune: %d", n)
	}
}

func TestLeaseRaceOneWinner(t *testing.T) {
	l := newLeases()
	now := time.Now()
	for round := 0; round < 50; round++ {
		key := fmt.Sprint("job-", round)
		var wins atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < 32; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				if ok, _ := l.claim(key, fmt.Sprint("dev-", g), time.Minute, now); ok {
					wins.Add(1)
				}
				l.holder(key, now)
				l.release(key, "nobody")
			}(g)
		}
		close(start)
		wg.Wait()
		if w := wins.Load(); w != 1 {
			t.Fatalf("round %d: %d winners", round, w)
		}
	}
	// Mixed keys, claims, releases and prunes under -race.
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := fmt.Sprint(i % 7)
				d := fmt.Sprint("d", g)
				l.claim(k, d, time.Millisecond, time.Now())
				l.release(k, d)
				if i%50 == 0 {
					l.prune(time.Now())
				}
			}
		}(g)
	}
	wg.Wait()
}
