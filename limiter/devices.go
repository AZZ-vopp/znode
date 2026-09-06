package limiter

import (
	"context"
	"sort"
	"sync"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/conf"
)

type deviceEntry struct {
	uid      int
	lastSeen time.Time
	// handoverStarted is non-zero only for the newly admitted address during
	// a Hysteria2 QUIC migration. It is deliberately not extended by traffic:
	// after one grace window the candidate must replace a silent old address or
	// be revoked, so two actively transmitting clients cannot keep a bypass.
	handoverStarted time.Time
	handoverBlocked bool
	lastRedisTouch  time.Time
	remoteApproved  bool
	// remoteAttempted separates a brand-new admission from retries after Redis
	// failed open. Only the first admission may block the caller; later retries
	// run in background so a Redis outage cannot pause the traffic writer.
	remoteAttempted bool
	remoteInFlight  bool
}

type deviceStore interface {
	Allow(context.Context, string, string, int) (bool, error)
}

type deviceRefresh struct {
	store   deviceStore
	userKey string
	ip      string
	limit   int
}

const deviceRefreshCooldown = 5 * time.Second

// deviceTracker is intentionally a bounded, TTL-based map. A connection from
// an existing IP updates one small entry; it does not allocate a new sync.Map
// as the previous implementation did for every handshake.
type deviceTracker struct {
	mu            sync.Mutex
	users         map[string]map[string]*deviceEntry
	ttl           time.Duration
	refresh       time.Duration
	grace         time.Duration
	handover      bool
	maxIPsPerUser int
	aliveMu       sync.RWMutex
	alive         map[int]int
	refreshQueue  chan deviceRefresh
	refreshCtx    context.Context
	refreshCancel context.CancelFunc
	refreshWG     sync.WaitGroup
	circuitUntil  time.Time
}

func newDeviceTracker(c *conf.GlobalDeviceLimitConfig) *deviceTracker {
	t := &deviceTracker{
		users:         make(map[string]map[string]*deviceEntry),
		alive:         make(map[int]int),
		ttl:           60 * time.Second,
		refresh:       20 * time.Second,
		grace:         15 * time.Second,
		maxIPsPerUser: 256,
	}
	if c != nil {
		applyDeviceDefaults(c)
		t.ttl = time.Duration(c.Expiry) * time.Second
		t.refresh = time.Duration(c.RefreshInterval) * time.Second
		t.grace = time.Duration(*c.HandoverGrace) * time.Second
		t.maxIPsPerUser = c.MaxIPsPerUser
		t.handover = c.CredentialHandover
	}
	t.refreshCtx, t.refreshCancel = context.WithCancel(context.Background())
	return t
}

func (t *deviceTracker) Observe(ctx context.Context, remote deviceStore, failClosed bool, userKey, ip string, uid, limit int, now time.Time) (bool, error) {
	t.mu.Lock()
	entries := t.users[userKey]
	if entries == nil {
		entries = make(map[string]*deviceEntry)
		t.users[userKey] = entries
	}
	t.pruneLocked(entries, now)
	entry, exists := entries[ip]
	if exists && entry.handoverBlocked {
		if !t.releaseBlockedHandoverLocked(entries, ip, now) {
			t.mu.Unlock()
			return false, nil
		}
		entry = entries[ip]
	}
	if exists && !entry.handoverStarted.IsZero() && !now.Before(entry.handoverStarted.Add(t.grace)) {
		if remote != nil {
			// Redis owns cross-node peer visibility. Force this touch through its
			// atomic candidate decision instead of promoting based on a partial
			// local view.
			entry.remoteApproved = false
			entry.remoteAttempted = false
		} else {
			if !t.finalizeHandoverLocked(entries, ip, now) {
				t.mu.Unlock()
				return false, nil
			}
			entry = entries[ip]
		}
	}
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
			if !t.evictHandoverLocked(entries, limit, now) {
				// Hysteria2 gets one temporary candidate (old IP + new IP). It
				// is revoked on a later traffic touch if the old IP is still
				// alive after grace. Never grow an overlap beyond two addresses.
				if !t.handover || t.grace <= 0 || len(entries) >= 2 {
					t.mu.Unlock()
					return false, nil
				}
				entry = &deviceEntry{uid: uid, handoverStarted: now}
				entries[ip] = entry
				goto observed
			}
		}
		entry = &deviceEntry{uid: uid}
		// A Redis-backed Hysteria2 limiter may be seeing the old address on a
		// different ZNode. Keep this new address provisional until Redis makes
		// the cross-node decision at the end of grace.
		if remote != nil && t.handover && limit == 1 && t.grace > 0 {
			entry.handoverStarted = now
		}
		entries[ip] = entry
	}
observed:
	entry.uid = uid
	entry.lastSeen = now
	requiresAdmission := remote != nil && limit > 0 && !entry.remoteApproved && !entry.remoteAttempted
	shouldTouchRedis := remote != nil && limit > 0 && (requiresAdmission || entry.lastRedisTouch.IsZero() || now.Sub(entry.lastRedisTouch) >= t.refresh)
	if shouldTouchRedis && !failClosed {
		if now.Before(t.circuitUntil) {
			// This entry was admitted under the explicit fail-open policy. Mark the
			// admission as attempted so its first packet after cooldown retries in
			// the background instead of stalling an established stream.
			entry.remoteAttempted = true
			t.mu.Unlock()
			return true, nil
		}
		// A concurrent TTL refresh may reuse the last successful admission. A
		// concurrent first admission may not: it must obtain Redis' atomic
		// decision too, otherwise a healthy denial could race with fail-open.
		if entry.remoteInFlight && (entry.remoteApproved || entry.remoteAttempted) {
			t.mu.Unlock()
			return true, nil
		}
		entry.remoteInFlight = true
		t.mu.Unlock()
		// A previously unseen IP is an admission decision, not a TTL refresh.
		// Ask healthy Redis synchronously so an over-limit device cannot keep an
		// already-admitted session after a background denial. One failed request
		// opens the circuit; subsequent handshakes fail open without repeating
		// the timeout until Redis has had time to recover.
		if requiresAdmission {
			allowed, err := remote.Allow(ctx, userKey, ip, limit)
			t.completeRefresh(userKey, ip, allowed, err)
			if err != nil {
				return true, err
			}
			return allowed, nil
		}
		if !t.enqueueRefresh(deviceRefresh{store: remote, userKey: userKey, ip: ip, limit: limit}) {
			// Queue pressure is not a successful Redis touch. Clear single-flight
			// state so a later packet can retry after workers catch up.
			t.cancelRefresh(userKey, ip)
		}
		return true, nil
	}
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

// finalizeHandoverLocked promotes a provisional new Hysteria2 address only
// after every other tracked address has gone silent. If another address is
// still moving data, the provisional (new) address is removed and its writer
// closes the flow. This preserves the credential's device limit after grace.
func (t *deviceTracker) finalizeHandoverLocked(entries map[string]*deviceEntry, candidateIP string, now time.Time) bool {
	candidate := entries[candidateIP]
	if candidate == nil {
		return false
	}
	silent := now.Add(-t.grace)
	for ip, entry := range entries {
		if ip != candidateIP && entry.lastSeen.After(silent) {
			candidate.handoverStarted = time.Time{}
			candidate.handoverBlocked = true
			return false
		}
	}
	for ip := range entries {
		if ip != candidateIP {
			delete(entries, ip)
		}
	}
	candidate.handoverStarted = time.Time{}
	return true
}

// releaseBlockedHandoverLocked prevents a rejected candidate from reconnecting
// immediately and receiving a fresh grace window. It can resume only once all
// prior addresses are silent, at which point it replaces them.
func (t *deviceTracker) releaseBlockedHandoverLocked(entries map[string]*deviceEntry, candidateIP string, now time.Time) bool {
	silent := now.Add(-t.grace)
	for ip, entry := range entries {
		if ip != candidateIP && entry.lastSeen.After(silent) {
			return false
		}
	}
	for ip := range entries {
		if ip != candidateIP {
			delete(entries, ip)
		}
	}
	entries[candidateIP].handoverBlocked = false
	return true
}

func (t *deviceTracker) startRemoteRefresh(workers int) {
	if workers < 1 {
		workers = 1
	}
	t.mu.Lock()
	if t.refreshQueue != nil {
		t.mu.Unlock()
		return
	}
	t.refreshQueue = make(chan deviceRefresh, 64)
	t.mu.Unlock()
	for i := 0; i < workers; i++ {
		t.refreshWG.Add(1)
		go func() {
			defer t.refreshWG.Done()
			for {
				select {
				case <-t.refreshCtx.Done():
					return
				case refresh := <-t.refreshQueue:
					allowed, err := refresh.store.Allow(t.refreshCtx, refresh.userKey, refresh.ip, refresh.limit)
					t.completeRefresh(refresh.userKey, refresh.ip, allowed, err)
				}
			}
		}()
	}
}

func (t *deviceTracker) enqueueRefresh(refresh deviceRefresh) bool {
	t.mu.Lock()
	queue := t.refreshQueue
	t.mu.Unlock()
	if queue == nil {
		return false
	}
	select {
	case queue <- refresh:
		return true
	default:
		return false
	}
}

func (t *deviceTracker) completeRefresh(userKey, ip string, allowed bool, err error) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil && now.After(t.circuitUntil) {
		t.circuitUntil = now.Add(deviceRefreshCooldown)
	}
	entries := t.users[userKey]
	if entries == nil || entries[ip] == nil {
		return
	}
	entry := entries[ip]
	entry.remoteInFlight = false
	entry.remoteAttempted = true
	if err == nil && allowed {
		entry.remoteApproved = true
		entry.lastRedisTouch = now
		if !entry.handoverStarted.IsZero() && !now.Before(entry.handoverStarted.Add(t.grace)) {
			entry.handoverStarted = time.Time{}
		}
		return
	}
	if err != nil {
		// Keep the local entry for bounded fail-open tracking, but let the
		// circuit breaker suppress remote retries until Redis has cooled down.
		return
	}
	delete(entries, ip)
	if len(entries) == 0 {
		delete(t.users, userKey)
	}
}

func (t *deviceTracker) cancelRefresh(userKey, ip string) {
	t.mu.Lock()
	if entries := t.users[userKey]; entries != nil {
		if entry := entries[ip]; entry != nil {
			entry.remoteInFlight = false
		}
	}
	t.mu.Unlock()
}

func (t *deviceTracker) Close() {
	if t == nil || t.refreshCancel == nil {
		return
	}
	t.refreshCancel()
	t.refreshWG.Wait()
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
			entry.remoteApproved = true
			entry.remoteAttempted = true
			entry.lastRedisTouch = now
			entry.remoteInFlight = false
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

// evictHandoverLocked frees room for one genuinely new address when enough of
// the tracked ones have been silent for at least the handover grace. lastSeen is
// stamped on every Observe, including the data-path touches, so a second client
// that is actually transmitting is never a candidate and still gets refused.
//
// Mirrors the Redis script: least recently seen first, and a set that is only
// partly idle refuses the newcomer rather than evicting an active address.
func (t *deviceTracker) evictHandoverLocked(entries map[string]*deviceEntry, limit int, now time.Time) bool {
	if t.grace <= 0 {
		return false
	}
	needed := len(entries) - limit + 1
	if needed <= 0 {
		return true
	}
	silent := now.Add(-t.grace)
	candidates := make([]string, 0, len(entries))
	for ip, entry := range entries {
		if entry.lastSeen.After(silent) {
			continue
		}
		candidates = append(candidates, ip)
	}
	if len(candidates) < needed {
		return false
	}
	sort.Slice(candidates, func(i, j int) bool {
		return entries[candidates[i]].lastSeen.Before(entries[candidates[j]].lastSeen)
	})
	for i := 0; i < needed; i++ {
		delete(entries, candidates[i])
	}
	return true
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
			if entry.handoverBlocked {
				continue
			}
			online = append(online, panel.OnlineUser{UID: entry.uid, IP: ip})
		}
	}
	return online, active
}
