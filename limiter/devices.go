package limiter

import (
	"context"
	"sync"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/conf"
)

type deviceEntry struct {
	uid            int
	lastSeen       time.Time
	lastRedisTouch time.Time
}

// deviceTracker is intentionally a bounded, TTL-based map. A connection from
// an existing IP updates one small entry; it does not allocate a new sync.Map
// as the previous implementation did for every handshake.
type deviceTracker struct {
	mu            sync.Mutex
	users         map[string]map[string]*deviceEntry
	ttl           time.Duration
	refresh       time.Duration
	maxIPsPerUser int
	aliveMu       sync.RWMutex
	alive         map[int]int
}

func newDeviceTracker(c *conf.GlobalDeviceLimitConfig) *deviceTracker {
	t := &deviceTracker{
		users:         make(map[string]map[string]*deviceEntry),
		alive:         make(map[int]int),
		ttl:           60 * time.Second,
		refresh:       20 * time.Second,
		maxIPsPerUser: 256,
	}
	if c != nil {
		applyDeviceDefaults(c)
		t.ttl = time.Duration(c.Expiry) * time.Second
		t.refresh = time.Duration(c.RefreshInterval) * time.Second
		t.maxIPsPerUser = c.MaxIPsPerUser
	}
	return t
}

func (t *deviceTracker) Observe(ctx context.Context, remote *redisDeviceStore, failClosed bool, userKey, ip string, uid, limit int, now time.Time) (bool, error) {
	t.mu.Lock()
	entries := t.users[userKey]
	if entries == nil {
		entries = make(map[string]*deviceEntry)
		t.users[userKey] = entries
	}
	t.pruneLocked(entries, now)
	entry, exists := entries[ip]
	if !exists {
		if t.maxIPsPerUser > 0 && len(entries) >= t.maxIPsPerUser {
			// Do not let a malformed/probing client grow this process without
			// bound. Traffic itself is still allowed when no device limit exists.
			if limit > 0 {
				t.mu.Unlock()
				return false, nil
			}
			t.mu.Unlock()
			return true, nil
		}
		// The panel alive count is a delayed aggregate and may still include this
		// same device after a ZNode reload. Using it for admission blocks the
		// first reconnect until the cache expires. Redis, when configured, owns
		// the cross-node exact-IP decision; otherwise enforce only the bounded
		// local set that this process can identify safely.
		if remote == nil && limit > 0 && len(entries) >= limit {
			t.mu.Unlock()
			return false, nil
		}
		entry = &deviceEntry{uid: uid}
		entries[ip] = entry
	}
	entry.uid = uid
	entry.lastSeen = now
	shouldTouchRedis := remote != nil && limit > 0 && (entry.lastRedisTouch.IsZero() || now.Sub(entry.lastRedisTouch) >= t.refresh)
	t.mu.Unlock()

	if !shouldTouchRedis {
		return true, nil
	}
	allowed, err := remote.Allow(ctx, userKey, ip, limit)
	if err != nil {
		if failClosed {
			t.removeEntry(userKey, ip)
			return false, err
		}
		t.markRedisTouch(userKey, ip, now)
		return true, err
	}
	if !allowed {
		t.removeEntry(userKey, ip)
		return false, nil
	}
	t.markRedisTouch(userKey, ip, now)
	return true, nil
}

func (t *deviceTracker) SetAliveList(alive map[int]int) {
	copyAlive := make(map[int]int, len(alive))
	for uid, count := range alive {
		if count > 0 {
			copyAlive[uid] = count
		}
	}
	t.aliveMu.Lock()
	t.alive = copyAlive
	t.aliveMu.Unlock()
}

func (t *deviceTracker) aliveCount(uid int) int {
	t.aliveMu.RLock()
	count := t.alive[uid]
	t.aliveMu.RUnlock()
	return count
}

func (t *deviceTracker) markRedisTouch(userKey, ip string, now time.Time) {
	t.mu.Lock()
	if entries := t.users[userKey]; entries != nil {
		if entry := entries[ip]; entry != nil {
			entry.lastRedisTouch = now
		}
	}
	t.mu.Unlock()
}

func (t *deviceTracker) removeEntry(userKey, ip string) {
	t.mu.Lock()
	if entries := t.users[userKey]; entries != nil {
		delete(entries, ip)
		if len(entries) == 0 {
			delete(t.users, userKey)
		}
	}
	t.mu.Unlock()
}

func (t *deviceTracker) Delete(userKey string) {
	t.mu.Lock()
	delete(t.users, userKey)
	t.mu.Unlock()
}

func (t *deviceTracker) pruneLocked(entries map[string]*deviceEntry, now time.Time) {
	cutoff := now.Add(-t.ttl)
	for ip, entry := range entries {
		if entry.lastSeen.Before(cutoff) {
			delete(entries, ip)
		}
	}
}

func (t *deviceTracker) Snapshot(now time.Time) ([]panel.OnlineUser, map[string]struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	online := make([]panel.OnlineUser, 0)
	active := make(map[string]struct{}, len(t.users))
	for userKey, entries := range t.users {
		t.pruneLocked(entries, now)
		if len(entries) == 0 {
			delete(t.users, userKey)
			continue
		}
		active[userKey] = struct{}{}
		for ip, entry := range entries {
			online = append(online, panel.OnlineUser{UID: entry.uid, IP: ip})
		}
	}
	return online, active
}
