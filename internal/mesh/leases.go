package mesh

import (
	"sync"
	"time"
)

const (
	maxLeaseTTL   = 24 * time.Hour
	maxLeaseCount = 100000
)

// leases is the in-memory claim table: job key -> holder until expiry.
// The first claimer wins until its lease expires; the holder may renew.
type leases struct {
	mu sync.Mutex
	m  map[string]lease
}

type lease struct {
	holder string
	exp    time.Time
}

func newLeases() *leases { return &leases{m: map[string]lease{}} }

// claim grants key to holder unless another holder has an unexpired lease.
// It returns whether it was granted and who holds the key now.
func (l *leases) claim(key, holder string, ttl time.Duration, now time.Time) (bool, string) {
	if ttl <= 0 {
		ttl = time.Second
	}
	if ttl > maxLeaseTTL {
		ttl = maxLeaseTTL
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.m[key]; ok && now.Before(cur.exp) && cur.holder != holder {
		return false, cur.holder
	}
	if len(l.m) >= maxLeaseCount {
		l.pruneLocked(now)
	}
	l.m[key] = lease{holder: holder, exp: now.Add(ttl)}
	return true, holder
}

// release drops key if holder holds it. It reports whether it did.
func (l *leases) release(key, holder string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.m[key]; ok && cur.holder == holder {
		delete(l.m, key)
		return true
	}
	return false
}

// holder returns the current holder of key, or "".
func (l *leases) holder(key string, now time.Time) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.m[key]; ok && now.Before(cur.exp) {
		return cur.holder
	}
	return ""
}

func (l *leases) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
}

func (l *leases) pruneLocked(now time.Time) {
	for k, v := range l.m {
		if !now.Before(v.exp) {
			delete(l.m, k)
		}
	}
}

func (l *leases) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}
