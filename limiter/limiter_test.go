package limiter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/common/format"
	"github.com/AZZ-vopp/znode/conf"
)

type fakeDeviceStore struct {
	calls atomic.Int32
	allow func(context.Context, string, string, int) (bool, error)
}

func (s *fakeDeviceStore) Allow(ctx context.Context, userKey, ip string, limit int) (bool, error) {
	s.calls.Add(1)
	return s.allow(ctx, userKey, ip, limit)
}

func (*fakeDeviceStore) Delete(context.Context, string) error { return nil }
func (*fakeDeviceStore) Close() error                         { return nil }

func waitForDeviceStoreCalls(t *testing.T, store *fakeDeviceStore, want int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if store.calls.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("remote calls = %d, want at least %d", store.calls.Load(), want)
}

func TestNormalizeIP(t *testing.T) {
	if got := normalizeIP("::ffff:192.0.2.10"); got != "192.0.2.10" {
		t.Fatalf("mapped IPv4 normalized to %q", got)
	}
	if got := normalizeIP("2001:db8:10:20:1234:5678:9abc:def0"); got != "2001:db8:10:20::" {
		t.Fatalf("global IPv6 /64 normalized to %q", got)
	}
	if got := normalizeIP("fe80::1234"); got != "fe80::1234" {
		t.Fatalf("link-local IPv6 normalized to %q", got)
	}
	if got := normalizeIP("not-an-ip"); got != "" {
		t.Fatalf("invalid IP normalized to %q", got)
	}
}

func TestClearDeviceIPsPreservesCredentialsAndTrafficState(t *testing.T) {
	Init()
	config := &conf.GlobalDeviceLimitConfig{MaxIPsPerCredential: 1}
	l := AddLimiter("vless", "clear-ip", []panel.UserInfo{
		{Id: 1, Uuid: "first-user", DeviceLimit: 1, SpeedLimit: 8},
		{Id: 2, Uuid: "second-user", DeviceLimit: 1, SpeedLimit: 8},
	}, nil, config, "https://panel.example")
	defer DeleteLimiter("clear-ip")
	first := format.UserTag("clear-ip", "first-user")
	second := format.UserTag("clear-ip", "second-user")
	if _, rejected := l.CheckLimit(context.Background(), first, "198.51.100.1"); rejected {
		t.Fatal("first credential was unexpectedly rejected")
	}
	if _, rejected := l.CheckLimit(context.Background(), second, "198.51.100.2"); rejected {
		t.Fatal("second credential was unexpectedly rejected")
	}

	firstDigest := format.UserCredentialDigest("first-user")
	l.ClearDeviceIPs("clear-ip", []string{fmt.Sprintf("%x", firstDigest)})
	if _, ok := l.devices.users[first]; ok {
		t.Fatal("target credential retained its IP state")
	}
	if _, ok := l.devices.users[second]; !ok {
		t.Fatal("unrelated credential lost its IP state")
	}
	if _, ok := l.UserLimitInfo.Load(first); !ok {
		t.Fatal("clearing IP state removed authorization")
	}

	l.ClearDeviceIPs("clear-ip", nil)
	if len(l.devices.users) != 0 || l.devices.totalEntries != 0 {
		t.Fatal("global clear retained device IP entries")
	}
	if _, ok := l.UserLimitInfo.Load(second); !ok {
		t.Fatal("global IP clear removed authorization")
	}
}

func TestDeviceLimitExcludedRelayIPsDoNotConsumeOrReportDevices(t *testing.T) {
	Init()
	zeroGrace := 0
	config := &conf.GlobalDeviceLimitConfig{MaxIPsPerCredential: 1, HandoverGrace: &zeroGrace}
	l := AddLimiter("vless", "relay-exclusion", []panel.UserInfo{{
		Id: 42, Uuid: "relay-user", DeviceLimit: 1, SpeedLimit: 8,
	}}, nil, config, "https://panel.example", []string{
		" ::ffff:198.51.100.10 ",
		"2001:db8:1:2:3:4:5:6",
		"not-an-ip",
	})
	defer DeleteLimiter("relay-exclusion")
	key := format.UserTag("relay-exclusion", "relay-user")

	if bucket, rejected := l.CheckLimit(context.Background(), key, "198.51.100.10"); rejected || bucket == nil {
		t.Fatal("configured relay IPv4 was rejected or lost the speed limiter")
	}
	if !l.TouchDevice(key, "::ffff:198.51.100.10") {
		t.Fatal("configured relay IPv4 was rejected during device refresh")
	}
	if !l.isDeviceLimitExcludedIP("2001:db8:1:2:3:4:5:6") || l.isDeviceLimitExcludedIP("2001:db8:1:2:3:4:5:7") {
		t.Fatal("relay IPv6 exclusion must use an exact canonical address")
	}
	deviceIP, excluded := l.PrepareDeviceIP("2001:db8:1:2:3:4:5:6")
	if !excluded || deviceIP != "2001:db8:1:2::" {
		t.Fatalf("prepared relay IPv6 = %q, excluded=%v", deviceIP, excluded)
	}
	if bucket, rejected := l.CheckLimit(context.Background(), key, "2001:db8:1:2:3:4:5:6"); rejected || bucket == nil {
		t.Fatal("configured relay IPv6 lost the per-user speed limiter")
	}
	if _, adjacentExcluded := l.PrepareDeviceIP("2001:db8:1:2:3:4:5:7"); adjacentExcluded {
		t.Fatal("exact relay exclusion broadened to the IPv6 /64")
	}

	if err := l.UpdateDeviceConfig(config, "https://panel.example"); err != nil {
		t.Fatalf("hot-swap config: %v", err)
	}
	if _, rejected := l.CheckLimit(context.Background(), key, "198.51.100.10"); rejected {
		t.Fatal("relay exclusion was lost after device config update")
	}
	if _, rejected := l.CheckLimit(context.Background(), key, "203.0.113.10"); rejected {
		t.Fatal("real client IP was unexpectedly rejected")
	}
	if _, rejected := l.CheckLimit(context.Background(), key, "203.0.113.11"); !rejected {
		t.Fatal("second real client IP bypassed the one-device limit")
	}

	online, err := l.GetOnlineDevice()
	if err != nil {
		t.Fatalf("online device snapshot: %v", err)
	}
	if len(*online) != 1 || (*online)[0].UID != 42 || (*online)[0].IP != "203.0.113.10" {
		t.Fatalf("relay IP leaked into online-device snapshot: %+v", *online)
	}
}

func TestExactIPv6RelayExclusionDoesNotBroadenOrSkipRedisRefresh(t *testing.T) {
	Init()
	zeroGrace := 0
	config := &conf.GlobalDeviceLimitConfig{MaxIPsPerCredential: 1, HandoverGrace: &zeroGrace}
	const relayIP = "2001:db8:1:2::"
	const adjacentIP = "2001:db8:1:2:abcd::1"
	l := AddLimiter("vless", "relay-redis-exclusion", []panel.UserInfo{{
		Id: 43, Uuid: "relay-redis-user", DeviceLimit: 1, SpeedLimit: 8,
	}}, nil, config, "https://panel.example", []string{relayIP})
	defer DeleteLimiter("relay-redis-exclusion")

	var seenIPs []string
	store := &fakeDeviceStore{allow: func(_ context.Context, _ string, ip string, _ int) (bool, error) {
		seenIPs = append(seenIPs, ip)
		return true, nil
	}}
	l.remote = store
	l.remoteEnabled = true
	l.failClosed = true
	key := format.UserTag("relay-redis-exclusion", "relay-redis-user")

	if _, rejected := l.CheckLimit(context.Background(), key, relayIP); rejected {
		t.Fatal("exact relay IPv6 was rejected")
	}
	if !l.TouchDevice(key, relayIP) {
		t.Fatal("exact relay IPv6 was rejected during refresh")
	}
	if got := store.calls.Load(); got != 0 {
		t.Fatalf("exact relay IPv6 made %d Redis calls, want 0", got)
	}

	normalized, excluded := l.PrepareDeviceIP(adjacentIP)
	if excluded || normalized != relayIP {
		t.Fatalf("adjacent IPv6 prepared as %q, excluded=%v", normalized, excluded)
	}
	if _, rejected := l.CheckLimit(context.Background(), key, adjacentIP); rejected {
		t.Fatal("adjacent real client IPv6 was rejected")
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("real client admission made %d Redis calls, want 1", got)
	}
	if len(seenIPs) != 1 || seenIPs[0] != relayIP {
		t.Fatalf("Redis admission IPs = %v, want normalized %q", seenIPs, relayIP)
	}

	l.devices.mu.Lock()
	l.devices.users[key][normalized].lastRedisTouch = time.Now().Add(-l.devices.refresh - time.Second)
	l.devices.mu.Unlock()
	if !l.TouchPreparedDevice(key, normalized) {
		t.Fatal("prepared real client IPv6 was rejected during refresh")
	}
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("prepared real client refresh made %d Redis calls, want 2 total", got)
	}
}

func TestRedisDeviceKeyUsesUUIDAndNamespaceHash(t *testing.T) {
	store := &redisDeviceStore{prefix: "znode:device", namespace: "https://panel.example/"}
	first := store.key("[node-a]|uuid-123")
	second := store.key("[node-b]|uuid-123")
	other := (&redisDeviceStore{prefix: "znode:device", namespace: "https://other.example"}).key("[node-a]|uuid-123")
	if first != second {
		t.Fatalf("same UUID on two nodes must share a Redis key: %q != %q", first, second)
	}
	if first == other || len(first) > 100 || containsRaw(first, "uuid-123") {
		t.Fatalf("Redis key should be namespaced and opaque: %q", first)
	}
}

func TestRedisDeviceKeyIsStableAcrossHashedUserTagUpgrade(t *testing.T) {
	store := &redisDeviceStore{prefix: "znode:device", namespace: "https://panel.example"}
	legacy := store.key("[node-a]|uuid-123")
	hardened := store.key(format.UserTag("[node-a]", "uuid-123"))
	if legacy != hardened {
		t.Fatalf("rolling upgrade split the device-limit identity: legacy=%q hardened=%q", legacy, hardened)
	}
}

func TestRedisDeviceStoresShareOneClientPerAgentConfig(t *testing.T) {
	config := &conf.GlobalDeviceLimitConfig{
		Enable:       true,
		RedisNetwork: "tcp",
		RedisAddr:    "127.0.0.1:6379",
		RedisDB:      7,
		Timeout:      2,
	}
	first, err := newRedisDeviceStore(config, "https://panel.example")
	if err != nil {
		t.Fatalf("create first store: %v", err)
	}
	second, err := newRedisDeviceStore(config, "https://panel.example")
	if err != nil {
		_ = first.Close()
		t.Fatalf("create second store: %v", err)
	}
	if first.client != second.client {
		t.Fatal("logical nodes with identical Redis settings did not share a client pool")
	}
	redisClientRegistry.Lock()
	shared := redisClientRegistry.clients[first.clientKey]
	refs := 0
	if shared != nil {
		refs = shared.refs
	}
	redisClientRegistry.Unlock()
	if refs != 2 {
		t.Fatalf("shared Redis client refs = %d, want 2", refs)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}
	if second.client == nil {
		t.Fatal("closing one logical node closed the shared client for the other node")
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second store: %v", err)
	}
	redisClientRegistry.Lock()
	remaining := len(redisClientRegistry.clients)
	redisClientRegistry.Unlock()
	if remaining != 0 {
		t.Fatalf("shared Redis registry retained %d clients after final close", remaining)
	}
}

func containsRaw(value, raw string) bool {
	for i := 0; i+len(raw) <= len(value); i++ {
		if value[i:i+len(raw)] == raw {
			return true
		}
	}
	return false
}

func TestDeviceTrackerEnforcesAndExpires(t *testing.T) {
	tracker := newDeviceTracker(nil)
	tracker.ttl = 20 * time.Millisecond
	now := time.Now()
	if allowed, err := tracker.Observe(context.Background(), nil, false, "user", "192.0.2.1", 1, 1, now); !allowed || err != nil {
		t.Fatalf("first device: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(context.Background(), nil, false, "user", "192.0.2.2", 1, 1, now); allowed || err != nil {
		t.Fatalf("second device should be rejected: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(context.Background(), nil, false, "user", "192.0.2.1", 1, 1, now); !allowed || err != nil {
		t.Fatalf("same device: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(context.Background(), nil, false, "user", "192.0.2.2", 1, 1, now.Add(25*time.Millisecond)); !allowed || err != nil {
		t.Fatalf("expired device should free a slot: allowed=%v err=%v", allowed, err)
	}
}

func TestDeviceTrackerBoundsUnlimitedUser(t *testing.T) {
	tracker := newDeviceTracker(nil)
	tracker.maxIPsPerUser = 2
	now := time.Now()
	for i, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"} {
		if allowed, err := tracker.Observe(context.Background(), nil, false, "user", ip, 1, 0, now); !allowed || err != nil {
			t.Fatalf("unlimited device %d: allowed=%v err=%v", i, allowed, err)
		}
	}
	online, _ := tracker.Snapshot(now)
	if len(online) != 2 {
		t.Fatalf("bounded tracker stored %d IPs, want 2", len(online))
	}
}

func TestDeviceTrackerDoesNotRejectReconnectFromStalePanelAliveCount(t *testing.T) {
	tracker := newDeviceTracker(nil)
	tracker.SetAliveList(map[int]int{42: 1})
	now := time.Now()
	if allowed, err := tracker.Observe(context.Background(), nil, false, "user", "192.0.2.9", 42, 1, now); !allowed || err != nil {
		t.Fatalf("stale panel alive count blocked the first reconnect: allowed=%v err=%v", allowed, err)
	}
}

func TestFailOpenDeviceAdmissionStillHonorsHealthyRedisDenial(t *testing.T) {
	tracker := newDeviceTracker(nil)
	store := &fakeDeviceStore{allow: func(context.Context, string, string, int) (bool, error) {
		return false, nil
	}}
	tracker.startRemoteRefresh(1)
	defer tracker.Close()

	for attempt := 0; attempt < 2; attempt++ {
		if allowed, err := tracker.Observe(context.Background(), store, false, "user", "192.0.2.1", 1, 1, time.Now()); allowed || err != nil {
			t.Fatalf("healthy Redis denial attempt %d: allowed=%v err=%v", attempt, allowed, err)
		}
	}
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("Redis admission calls = %d, want 2", got)
	}
}

func TestFailOpenConcurrentSameIPCannotBypassHealthyRedisDenial(t *testing.T) {
	tracker := newDeviceTracker(nil)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	store := &fakeDeviceStore{allow: func(context.Context, string, string, int) (bool, error) {
		started <- struct{}{}
		<-release
		return false, nil
	}}
	tracker.startRemoteRefresh(1)
	defer tracker.Close()

	type result struct {
		allowed bool
		err     error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			allowed, err := tracker.Observe(context.Background(), store, false, "user", "192.0.2.1", 1, 1, time.Now())
			results <- result{allowed: allowed, err: err}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("concurrent admission did not wait for Redis")
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		result := <-results
		if result.allowed || result.err != nil {
			t.Fatalf("concurrent denial result %d: allowed=%v err=%v", i, result.allowed, result.err)
		}
	}
}

func TestFailOpenApprovedDeviceRefreshIsNonblockingAndSingleFlightPerIP(t *testing.T) {
	tracker := newDeviceTracker(nil)
	started := make(chan struct{}, 1)
	var block atomic.Bool
	store := &fakeDeviceStore{allow: func(ctx context.Context, _, _ string, _ int) (bool, error) {
		if !block.Load() {
			return true, nil
		}
		started <- struct{}{}
		<-ctx.Done()
		return true, ctx.Err()
	}}
	tracker.startRemoteRefresh(1)
	defer tracker.Close()
	now := time.Now()
	if allowed, err := tracker.Observe(context.Background(), store, false, "user", "192.0.2.1", 1, 1, now); !allowed || err != nil {
		t.Fatalf("initial Redis admission: allowed=%v err=%v", allowed, err)
	}
	block.Store(true)
	now = time.Now().Add(tracker.refresh + time.Second)
	start := time.Now()
	for i := 0; i < 20; i++ {
		if allowed, err := tracker.Observe(context.Background(), store, false, "user", "192.0.2.1", 1, 1, now); !allowed || err != nil {
			t.Fatalf("fail-open observe %d: allowed=%v err=%v", i, allowed, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("fail-open path blocked for %s", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not start")
	}
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("same IP created %d total Redis calls, want admission + one refresh", got)
	}
}

func TestFailOpenDeviceRefreshOpensCircuitAfterRedisError(t *testing.T) {
	tracker := newDeviceTracker(nil)
	store := &fakeDeviceStore{allow: func(context.Context, string, string, int) (bool, error) {
		return false, errors.New("redis unavailable")
	}}
	tracker.startRemoteRefresh(1)
	defer tracker.Close()
	now := time.Now()
	if allowed, err := tracker.Observe(context.Background(), store, false, "user", "192.0.2.1", 1, 2, now); !allowed || err == nil {
		t.Fatalf("initial Redis failure should fail open and report the error: allowed=%v err=%v", allowed, err)
	}
	waitForDeviceStoreCalls(t, store, 1)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		tracker.mu.Lock()
		open := time.Now().Before(tracker.circuitUntil)
		tracker.mu.Unlock()
		if open {
			break
		}
		time.Sleep(time.Millisecond)
	}
	tracker.mu.Lock()
	open := time.Now().Before(tracker.circuitUntil)
	tracker.mu.Unlock()
	if !open {
		t.Fatal("Redis error did not open cooldown circuit")
	}
	if allowed, err := tracker.Observe(context.Background(), store, false, "user", "192.0.2.2", 1, 2, time.Now()); !allowed || err != nil {
		t.Fatalf("circuit fail-open observe: allowed=%v err=%v", allowed, err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("cooldown made %d remote calls, want 1", got)
	}

	tracker.mu.Lock()
	tracker.circuitUntil = time.Now().Add(-time.Millisecond)
	tracker.mu.Unlock()
	store.allow = func(context.Context, string, string, int) (bool, error) { return false, nil }
	if allowed, err := tracker.Observe(context.Background(), store, false, "user", "192.0.2.2", 1, 2, time.Now()); !allowed || err != nil {
		t.Fatalf("fail-open recovery retry blocked existing traffic: allowed=%v err=%v", allowed, err)
	}
	waitForDeviceStoreCalls(t, store, 2)
	if allowed, err := tracker.Observe(context.Background(), store, false, "user", "192.0.2.2", 1, 2, time.Now()); allowed || err != nil {
		t.Fatalf("healthy Redis denial was not enforced after background recovery: allowed=%v err=%v", allowed, err)
	}
}

func TestFailOpenRedisRetryNeverBlocksExistingTrafficAfterAdmissionError(t *testing.T) {
	tracker := newDeviceTracker(nil)
	tracker.refresh = time.Millisecond
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	store := &fakeDeviceStore{allow: func(ctx context.Context, _ string, _ string, _ int) (bool, error) {
		if first.CompareAndSwap(true, false) {
			return false, errors.New("redis unavailable")
		}
		started <- struct{}{}
		select {
		case <-release:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}}
	tracker.startRemoteRefresh(1)
	defer tracker.Close()

	now := time.Now()
	if allowed, err := tracker.Observe(context.Background(), store, false, "user", "192.0.2.1", 1, 1, now); !allowed || err == nil {
		t.Fatalf("initial Redis failure should fail open: allowed=%v err=%v", allowed, err)
	}
	tracker.mu.Lock()
	tracker.circuitUntil = time.Now().Add(-time.Millisecond)
	tracker.mu.Unlock()

	start := time.Now()
	if allowed, err := tracker.Observe(context.Background(), store, false, "user", "192.0.2.1", 1, 1, now.Add(time.Second)); !allowed || err != nil {
		t.Fatalf("existing traffic retry: allowed=%v err=%v", allowed, err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("existing traffic blocked on Redis retry for %s", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Redis retry did not move to the background worker")
	}
	close(release)
}

func TestRedisHandoverDenialAfterGraceIsSynchronous(t *testing.T) {
	tracker := newDeviceTracker(&conf.GlobalDeviceLimitConfig{CredentialHandover: true})
	tracker.ttl = time.Second
	tracker.grace = 20 * time.Millisecond
	var calls atomic.Int32
	store := &fakeDeviceStore{allow: func(context.Context, string, string, int) (bool, error) {
		// Admit the incumbent and provisional candidate, then keep the incumbent
		// active. Redis must synchronously reject the candidate after its grace.
		return calls.Add(1) < 4, nil
	}}
	tracker.startRemoteRefresh(1)
	defer tracker.Close()

	now := time.Now()
	ctx := context.Background()
	if allowed, err := tracker.Observe(ctx, store, false, "user", "192.0.2.1", 1, 1, now); !allowed || err != nil {
		t.Fatalf("incumbent admission: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(ctx, store, false, "user", "192.0.2.2", 1, 1, now.Add(time.Millisecond)); !allowed || err != nil {
		t.Fatalf("candidate admission: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(ctx, store, false, "user", "192.0.2.1", 1, 1, now.Add(21*time.Millisecond)); !allowed || err != nil {
		t.Fatalf("incumbent refresh: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(ctx, store, false, "user", "192.0.2.2", 1, 1, now.Add(25*time.Millisecond)); allowed || err != nil {
		t.Fatalf("candidate handover denial: allowed=%v err=%v", allowed, err)
	}
	if got := store.calls.Load(); got != 4 {
		t.Fatalf("Redis handover calls = %d, want 4 synchronous decisions", got)
	}
}

func TestDeviceLimiterHonorsFailClosedWhenRedisCannotInitialize(t *testing.T) {
	Init()
	config := &conf.GlobalDeviceLimitConfig{
		Enable:       true,
		RedisNetwork: "unsupported",
		FailClosed:   true,
	}
	limiter := AddLimiter("vless", "node-a", []panel.UserInfo{{
		Id: 1, Uuid: "uuid-a", DeviceLimit: 1,
	}}, nil, config, "https://panel.example")
	defer DeleteLimiter("node-a")

	if _, rejected := limiter.CheckLimit(
		context.Background(), format.UserTag("node-a", "uuid-a"), "192.0.2.10",
	); !rejected {
		t.Fatal("FailClosed=true allowed a device-limited session without Redis")
	}
}

func TestCredentialIPFanoutAllowsMobileCarrierNATAliases(t *testing.T) {
	Init()
	l := AddLimiter("vless", "mobile-nat", []panel.UserInfo{{
		Id: 7, Uuid: "uuid-mobile", DeviceLimit: 1,
	}}, nil, nil, "https://panel.example")
	defer DeleteLimiter("mobile-nat")
	key := format.UserTag("mobile-nat", "uuid-mobile")
	for _, ip := range []string{
		"111.55.79.165",
		"111.55.79.141",
		"111.55.79.149",
		"111.55.79.135",
	} {
		if _, rejected := l.CheckLimit(context.Background(), key, ip); rejected {
			t.Fatalf("mobile NAT alias %s was rejected", ip)
		}
	}
	if _, rejected := l.CheckLimit(context.Background(), key, "111.55.79.131"); !rejected {
		t.Fatal("fifth concurrent IP alias exceeded the configured fanout")
	}
}

func TestDeviceLimitBypassKeepsSpeedLimitAndReenablesAdmission(t *testing.T) {
	Init()
	zeroGrace := 0
	l := AddLimiter("vless", "node-b", []panel.UserInfo{{
		Id: 1, Uuid: "uuid-b", DeviceLimit: 1, SpeedLimit: 10,
	}}, nil, &conf.GlobalDeviceLimitConfig{MaxIPsPerCredential: 1, HandoverGrace: &zeroGrace}, "https://panel.example")
	defer DeleteLimiter("node-b")

	key := format.UserTag("node-b", "uuid-b")
	if _, rejected := l.CheckLimit(context.Background(), key, "192.0.2.1"); rejected {
		t.Fatal("first local device was rejected")
	}
	if _, rejected := l.CheckLimit(context.Background(), key, "192.0.2.2"); !rejected {
		t.Fatal("second local device was not rejected before bypass")
	}
	if !l.SetDeviceLimitBypass(true) || !l.DeviceLimitBypassed() {
		t.Fatal("device limit bypass did not transition on")
	}
	bucket, rejected := l.CheckLimit(context.Background(), key, "192.0.2.2")
	if rejected || bucket == nil {
		t.Fatalf("bypass did not retain speed limiting: rejected=%v bucket=%v", rejected, bucket)
	}
	if !l.TouchDevice(key, "192.0.2.99") {
		t.Fatal("TouchDevice was not bypassed")
	}
	if !l.SetDeviceLimitBypass(false) || l.DeviceLimitBypassed() {
		t.Fatal("device limit bypass did not transition off")
	}
	if _, rejected := l.CheckLimit(context.Background(), key, "192.0.2.2"); !rejected {
		t.Fatal("device limit was not enforced immediately after re-enable")
	}
}

func TestPanelOutageBypassStillEnforcesRedisAndFailClosed(t *testing.T) {
	for _, test := range []struct {
		name            string
		remoteAvailable bool
		remoteEnabled   bool
		failClosed      bool
		wantEnforced    bool
	}{
		{name: "healthy Redis fail open policy", remoteAvailable: true, remoteEnabled: true, wantEnforced: true},
		{name: "healthy Redis fail closed policy", remoteAvailable: true, remoteEnabled: true, failClosed: true, wantEnforced: true},
		{name: "unavailable Redis fail closed policy", remoteEnabled: true, failClosed: true, wantEnforced: true},
		{name: "unavailable Redis fail open policy", remoteEnabled: true, wantEnforced: false},
		{name: "local tracker", wantEnforced: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldEnforceDeviceLimit(true, test.remoteAvailable, test.remoteEnabled, test.failClosed); got != test.wantEnforced {
				t.Fatalf("enforced=%v, want %v", got, test.wantEnforced)
			}
		})
	}
	if !shouldEnforceDeviceLimit(false, false, false, false) {
		t.Fatal("normal local device admission was disabled")
	}
}

func TestPanelOutageBypassUsesHealthyRedisDecisionAndKeepsSpeedLimit(t *testing.T) {
	key := format.UserTag("redis-outage", "uuid")
	store := &fakeDeviceStore{allow: func(context.Context, string, string, int) (bool, error) {
		return false, nil
	}}
	l := &Limiter{
		UserLimitInfo: new(sync.Map), SpeedLimiter: new(sync.Map),
		devices: newDeviceTracker(nil), remote: store, remoteEnabled: true, failClosed: true,
	}
	defer l.devices.Close()
	l.UserLimitInfo.Store(key, UserLimitInfo{UID: 1, DeviceLimit: 1, SpeedLimit: 10})
	l.SetDeviceLimitBypass(true)
	if bucket, rejected := l.CheckLimit(context.Background(), key, "192.0.2.1"); !rejected || bucket != nil {
		t.Fatalf("healthy Redis denial was bypassed: rejected=%v bucket=%v", rejected, bucket)
	}
	store.allow = func(context.Context, string, string, int) (bool, error) { return true, nil }
	if bucket, rejected := l.CheckLimit(context.Background(), key, "192.0.2.1"); rejected || bucket == nil {
		t.Fatalf("healthy Redis admission lost speed limit: rejected=%v bucket=%v", rejected, bucket)
	}
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("Redis calls=%d, want denial and admission", got)
	}
}

func TestDeviceTrackerGlobalBoundsAndTTLRecovery(t *testing.T) {
	tracker := newDeviceTracker(&conf.GlobalDeviceLimitConfig{
		Expiry: 10, MaxIPsPerUser: 4, MaxIPsPerCredential: 1,
	})
	tracker.maxCredentials = 2
	tracker.maxEntries = 2
	now := time.Now()
	for _, user := range []string{"limited-a", "limited-b"} {
		if allowed, err := tracker.Observe(context.Background(), nil, false, user, "192.0.2.1", 1, 1, now); !allowed || err != nil {
			t.Fatalf("seed %s: allowed=%v err=%v", user, allowed, err)
		}
	}
	if allowed, err := tracker.Observe(context.Background(), nil, false, "limited-c", "192.0.2.3", 3, 1, now); allowed || err != nil {
		t.Fatalf("limited admission at global bound: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(context.Background(), nil, false, "unlimited", "192.0.2.4", 4, 0, now); !allowed || err != nil {
		t.Fatalf("unlimited traffic at global bound: allowed=%v err=%v", allowed, err)
	}
	if len(tracker.users) != 2 || tracker.totalEntries != 2 {
		t.Fatalf("tracker exceeded global bound: credentials=%d entries=%d", len(tracker.users), tracker.totalEntries)
	}
	if allowed, err := tracker.Observe(context.Background(), nil, false, "limited-c", "192.0.2.3", 3, 1, now.Add(11*time.Second)); !allowed || err != nil {
		t.Fatalf("TTL cleanup did not free global capacity: allowed=%v err=%v", allowed, err)
	}
	if len(tracker.users) != 1 || tracker.totalEntries != 1 {
		t.Fatalf("expired global state was retained: credentials=%d entries=%d", len(tracker.users), tracker.totalEntries)
	}
}

func TestDeviceTrackerGlobalEntryBoundFailsSafeWithoutTrackingUnlimitedTraffic(t *testing.T) {
	tracker := newDeviceTracker(&conf.GlobalDeviceLimitConfig{MaxIPsPerUser: 8, MaxIPsPerCredential: 1})
	tracker.maxCredentials = 8
	tracker.maxEntries = 2
	now := time.Now()
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		if allowed, err := tracker.Observe(context.Background(), nil, false, "shared", ip, 1, 8, now); !allowed || err != nil {
			t.Fatalf("seed entry %s: allowed=%v err=%v", ip, allowed, err)
		}
	}
	if allowed, err := tracker.Observe(context.Background(), nil, false, "shared", "192.0.2.3", 1, 8, now); allowed || err != nil {
		t.Fatalf("limited entry at global bound: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(context.Background(), nil, false, "unlimited", "192.0.2.4", 2, 0, now); !allowed || err != nil {
		t.Fatalf("unlimited entry at global bound: allowed=%v err=%v", allowed, err)
	}
	if tracker.totalEntries != 2 || len(tracker.users) != 1 {
		t.Fatalf("entry bound grew tracker: credentials=%d entries=%d", len(tracker.users), tracker.totalEntries)
	}
}

func TestDeviceTrackerGlobalBoundConcurrentAdmissions(t *testing.T) {
	tracker := newDeviceTracker(&conf.GlobalDeviceLimitConfig{
		MaxIPsPerUser: 4, MaxIPsPerCredential: 1,
	})
	tracker.maxCredentials = 8
	tracker.maxEntries = 8
	const attempts = 64
	var wg sync.WaitGroup
	var allowed atomic.Int32
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("user-%d", i)
			ip := fmt.Sprintf("192.0.2.%d", i+1)
			ok, err := tracker.Observe(context.Background(), nil, false, user, ip, i, 1, time.Now())
			if err != nil {
				t.Errorf("observe %d: %v", i, err)
			}
			if ok {
				allowed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if got := allowed.Load(); got != 8 {
		t.Fatalf("limited concurrent admissions=%d, want 8", got)
	}
	if len(tracker.users) > 8 || tracker.totalEntries > 8 {
		t.Fatalf("tracker exceeded concurrent bound: credentials=%d entries=%d", len(tracker.users), tracker.totalEntries)
	}
}

func TestDeviceConfigHotSwapPreservesLocalAdmissions(t *testing.T) {
	Init()
	zeroGrace := 0
	config := &conf.GlobalDeviceLimitConfig{
		MaxIPsPerCredential: 2,
		HandoverGrace:       &zeroGrace,
	}
	l := AddLimiter("vless", "hot-swap", []panel.UserInfo{{
		Id: 9, Uuid: "uuid-hot-swap", DeviceLimit: 1,
	}}, nil, config, "https://panel.example")
	defer DeleteLimiter("hot-swap")
	key := format.UserTag("hot-swap", "uuid-hot-swap")

	if _, rejected := l.CheckLimit(context.Background(), key, "192.0.2.1"); rejected {
		t.Fatal("first device was rejected")
	}
	if err := l.UpdateDeviceConfig(config, "https://panel.example"); err != nil {
		t.Fatalf("hot-swap local config: %v", err)
	}
	if _, rejected := l.CheckLimit(context.Background(), key, "192.0.2.2"); rejected {
		t.Fatal("second configured IP alias was rejected after hot-swap")
	}
	if _, rejected := l.CheckLimit(context.Background(), key, "192.0.2.3"); !rejected {
		t.Fatal("hot-swap discarded existing admissions and allowed an extra alias")
	}
}

func TestDeviceConfigHotSwapKeepsFailClosedForEstablishedFlow(t *testing.T) {
	Init()
	l := AddLimiter("vless", "hot-swap-fail-closed", []panel.UserInfo{{
		Id: 10, Uuid: "uuid-fail-closed", DeviceLimit: 1,
	}}, nil, nil, "https://panel.example")
	defer DeleteLimiter("hot-swap-fail-closed")
	key := format.UserTag("hot-swap-fail-closed", "uuid-fail-closed")
	if _, rejected := l.CheckLimit(context.Background(), key, "192.0.2.10"); rejected {
		t.Fatal("initial local device was rejected")
	}

	err := l.UpdateDeviceConfig(&conf.GlobalDeviceLimitConfig{
		Enable: true, RedisNetwork: "unsupported", FailClosed: true,
	}, "https://panel.example")
	if err == nil {
		t.Fatal("invalid Redis hot-swap unexpectedly initialized")
	}
	if l.TouchDevice(key, "192.0.2.10") {
		t.Fatal("established flow bypassed fail-closed after Redis hot-swap failed")
	}
}

func BenchmarkDeviceTrackerSameIP(b *testing.B) {
	tracker := newDeviceTracker(nil)
	now := time.Now()
	if allowed, err := tracker.Observe(context.Background(), nil, false, "user", "192.0.2.1", 1, 1, now); !allowed || err != nil {
		b.Fatalf("seed device: allowed=%v err=%v", allowed, err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if allowed, err := tracker.Observe(context.Background(), nil, false, "user", "192.0.2.1", 1, 1, now); !allowed || err != nil {
			b.Fatal("same device was rejected")
		}
	}
}

func TestDeviceTrackerHandsOverSlotAfterGraceOfSilence(t *testing.T) {
	tracker := newDeviceTracker(nil)
	tracker.ttl = 10 * time.Second
	tracker.grace = 20 * time.Millisecond
	now := time.Now()
	ctx := context.Background()
	if allowed, err := tracker.Observe(ctx, nil, false, "user", "192.0.2.1", 1, 1, now); !allowed || err != nil {
		t.Fatalf("first address: allowed=%v err=%v", allowed, err)
	}
	// Still transmitting, so the slot is genuinely taken.
	if allowed, err := tracker.Observe(ctx, nil, false, "user", "192.0.2.2", 1, 1, now); allowed || err != nil {
		t.Fatalf("second address while the first is active: allowed=%v err=%v", allowed, err)
	}
	// WiFi went down: the old address stops refreshing and the new one takes over
	// well before the TTL would have released it.
	if allowed, err := tracker.Observe(ctx, nil, false, "user", "192.0.2.2", 1, 1, now.Add(30*time.Millisecond)); !allowed || err != nil {
		t.Fatalf("handover after the grace: allowed=%v err=%v", allowed, err)
	}
	online, _ := tracker.Snapshot(now.Add(30 * time.Millisecond))
	if len(online) != 1 || online[0].IP != "192.0.2.2" {
		t.Fatalf("the silent address was not released: %+v", online)
	}
}

func TestDeviceTrackerStillDeniesAConcurrentlyActiveSecondAddress(t *testing.T) {
	tracker := newDeviceTracker(nil)
	tracker.ttl = 10 * time.Second
	tracker.grace = 20 * time.Millisecond
	now := time.Now()
	ctx := context.Background()
	if allowed, err := tracker.Observe(ctx, nil, false, "user", "192.0.2.1", 1, 1, now); !allowed || err != nil {
		t.Fatalf("first address: allowed=%v err=%v", allowed, err)
	}
	// The incumbent keeps moving traffic, which restamps lastSeen past the grace.
	if allowed, err := tracker.Observe(ctx, nil, false, "user", "192.0.2.1", 1, 1, now.Add(25*time.Millisecond)); !allowed || err != nil {
		t.Fatalf("incumbent refresh: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(ctx, nil, false, "user", "192.0.2.2", 1, 1, now.Add(30*time.Millisecond)); allowed || err != nil {
		t.Fatalf("sharing must still be refused: allowed=%v err=%v", allowed, err)
	}
}

func TestDeviceTrackerHandoverFreesOnlyTheSilentSlotWhenLimitIsTwo(t *testing.T) {
	tracker := newDeviceTracker(nil)
	tracker.ttl = 10 * time.Second
	tracker.grace = 20 * time.Millisecond
	now := time.Now()
	ctx := context.Background()
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		if allowed, err := tracker.Observe(ctx, nil, false, "user", ip, 1, 2, now); !allowed || err != nil {
			t.Fatalf("address %s: allowed=%v err=%v", ip, allowed, err)
		}
	}
	// .1 keeps transmitting while .2 goes quiet. A third address may take the
	// quiet slot: two addresses are still the most that run at once, which is
	// what the limit of two allows.
	if allowed, err := tracker.Observe(ctx, nil, false, "user", "192.0.2.1", 1, 2, now.Add(25*time.Millisecond)); !allowed || err != nil {
		t.Fatalf("incumbent refresh: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(ctx, nil, false, "user", "192.0.2.3", 1, 2, now.Add(30*time.Millisecond)); !allowed || err != nil {
		t.Fatalf("the quiet slot should have been handed over: allowed=%v err=%v", allowed, err)
	}
	online, _ := tracker.Snapshot(now.Add(30 * time.Millisecond))
	if len(online) != 2 {
		t.Fatalf("handover freed the wrong number of slots: %+v", online)
	}
	// The address that was still transmitting kept its slot; the quiet one lost it.
	held := map[string]bool{}
	for _, entry := range online {
		held[entry.IP] = true
	}
	if !held["192.0.2.1"] || !held["192.0.2.3"] || held["192.0.2.2"] {
		t.Fatalf("the wrong address was evicted: %+v", online)
	}
}

func TestDeviceTrackerRefusesAThirdAddressWhileBothSlotsAreActive(t *testing.T) {
	tracker := newDeviceTracker(nil)
	tracker.ttl = 10 * time.Second
	tracker.grace = 20 * time.Millisecond
	now := time.Now()
	ctx := context.Background()
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		if allowed, err := tracker.Observe(ctx, nil, false, "user", ip, 1, 2, now); !allowed || err != nil {
			t.Fatalf("address %s: allowed=%v err=%v", ip, allowed, err)
		}
	}
	// Both keep moving traffic, so neither slot is available and genuine sharing
	// by a third client is still refused.
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		if allowed, err := tracker.Observe(ctx, nil, false, "user", ip, 1, 2, now.Add(25*time.Millisecond)); !allowed || err != nil {
			t.Fatalf("refresh of %s: allowed=%v err=%v", ip, allowed, err)
		}
	}
	if allowed, err := tracker.Observe(ctx, nil, false, "user", "192.0.2.3", 1, 2, now.Add(30*time.Millisecond)); allowed || err != nil {
		t.Fatalf("sharing must still be refused: allowed=%v err=%v", allowed, err)
	}
}

func TestDeviceTrackerHandoverIsDisabledByAZeroGrace(t *testing.T) {
	tracker := newDeviceTracker(nil)
	tracker.ttl = 10 * time.Second
	tracker.grace = 0
	now := time.Now()
	ctx := context.Background()
	if allowed, err := tracker.Observe(ctx, nil, false, "user", "192.0.2.1", 1, 1, now); !allowed || err != nil {
		t.Fatalf("first address: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(ctx, nil, false, "user", "192.0.2.2", 1, 1, now.Add(5*time.Second)); allowed || err != nil {
		t.Fatalf("a zero grace must keep the previous refusal: allowed=%v err=%v", allowed, err)
	}
}

func TestHysteria2CredentialHandoverLetsWiFiMoveToMobile(t *testing.T) {
	tracker := newDeviceTracker(&conf.GlobalDeviceLimitConfig{CredentialHandover: true})
	tracker.ttl = time.Second
	tracker.grace = 20 * time.Millisecond
	now := time.Now()
	ctx := context.Background()
	if allowed, err := tracker.Observe(ctx, nil, false, "device-uuid", "192.0.2.10", 1, 1, now); !allowed || err != nil {
		t.Fatalf("Wi-Fi admission: allowed=%v err=%v", allowed, err)
	}
	// The new mobile address is admitted provisionally so an in-flight QUIC
	// migration does not lose traffic while the Wi-Fi path drains.
	if allowed, err := tracker.Observe(ctx, nil, false, "device-uuid", "198.51.100.20", 1, 1, now.Add(time.Millisecond)); !allowed || err != nil {
		t.Fatalf("mobile provisional admission: allowed=%v err=%v", allowed, err)
	}
	// Wi-Fi is silent. After grace, the mobile address is promoted and replaces
	// it without waiting for the full TTL.
	if allowed, err := tracker.Observe(ctx, nil, false, "device-uuid", "198.51.100.20", 1, 1, now.Add(25*time.Millisecond)); !allowed || err != nil {
		t.Fatalf("mobile promotion: allowed=%v err=%v", allowed, err)
	}
	online, _ := tracker.Snapshot(now.Add(25 * time.Millisecond))
	if len(online) != 1 || online[0].IP != "198.51.100.20" {
		t.Fatalf("handover did not replace Wi-Fi address: %+v", online)
	}
}

func TestHysteria2CredentialHandoverRejectsConcurrentClientsAfterGrace(t *testing.T) {
	tracker := newDeviceTracker(&conf.GlobalDeviceLimitConfig{CredentialHandover: true})
	tracker.ttl = time.Second
	tracker.grace = 20 * time.Millisecond
	now := time.Now()
	ctx := context.Background()
	if allowed, err := tracker.Observe(ctx, nil, false, "device-uuid", "192.0.2.10", 1, 1, now); !allowed || err != nil {
		t.Fatalf("first client: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(ctx, nil, false, "device-uuid", "198.51.100.20", 1, 1, now.Add(time.Millisecond)); !allowed || err != nil {
		t.Fatalf("provisional second client: allowed=%v err=%v", allowed, err)
	}
	// The incumbent is still actively sending. The new address must lose its
	// probation rather than receiving an endlessly renewed grace window.
	if allowed, err := tracker.Observe(ctx, nil, false, "device-uuid", "192.0.2.10", 1, 1, now.Add(21*time.Millisecond)); !allowed || err != nil {
		t.Fatalf("incumbent traffic: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := tracker.Observe(ctx, nil, false, "device-uuid", "198.51.100.20", 1, 1, now.Add(25*time.Millisecond)); allowed || err != nil {
		t.Fatalf("concurrent candidate remained admitted: allowed=%v err=%v", allowed, err)
	}
	// A reconnect from the rejected address cannot manufacture another grace
	// period while the incumbent is still active.
	if allowed, err := tracker.Observe(ctx, nil, false, "device-uuid", "198.51.100.20", 1, 1, now.Add(30*time.Millisecond)); allowed || err != nil {
		t.Fatalf("blocked candidate bypassed with reconnect: allowed=%v err=%v", allowed, err)
	}
}

func TestHysteria2IPv6PrivacyRotationKeepsNetworkIdentity(t *testing.T) {
	tracker := newDeviceTracker(&conf.GlobalDeviceLimitConfig{CredentialHandover: true})
	now := time.Now()
	for index, ip := range []string{"2001:db8:100:7::1", "2001:db8:100:7:abcd::20", "2001:db8:100:7:ffff::30"} {
		if allowed, err := tracker.Observe(context.Background(), nil, false, "device-uuid", normalizeIP(ip), 1, 1, now.Add(time.Duration(index)*time.Millisecond)); !allowed || err != nil {
			t.Fatalf("IPv6 privacy rotation must not consume another device slot: allowed=%v err=%v", allowed, err)
		}
	}
	online, _ := tracker.Snapshot(now.Add(2 * time.Millisecond))
	if len(online) != 1 || online[0].IP != "2001:db8:100:7::" {
		t.Fatalf("unexpected address grouping: %+v", online)
	}
}

func TestCredentialHandoverIsEnabledForCredentialProtocols(t *testing.T) {
	Init()
	for _, protocol := range []string{"vless", "trojan", "vmess", "hysteria2"} {
		tag := protocol + "-node"
		l := AddLimiter(protocol, tag, nil, nil, nil, "https://panel.example")
		defer DeleteLimiter(tag)
		if !l.devices.handover {
			t.Fatalf("credential handover was not enabled for %s", protocol)
		}
		if l.remote != nil || l.remoteEnabled {
			t.Fatalf("local-only %s handover unexpectedly enabled Redis", protocol)
		}
	}
}

func TestRedisHandoverScriptHasTheSameRevocableOverlapContract(t *testing.T) {
	// This guards the Redis half of the algorithm without requiring a Redis
	// daemon in unit tests. The Lua state names are intentionally checked here:
	// P is a fixed-start probation; B blocks reconnects until the old traffic is
	// silent. Both are necessary for parity with deviceTracker.
	for _, fragment := range []string{
		"handoverEnabled == 1 and count < 2",
		"'P:' .. now",
		"state == 'B'",
		"redis.call('HSET', handoverKey, member, 'B')",
		"redis.call('HDEL', handoverKey, member)",
	} {
		if !strings.Contains(deviceLimitScript, fragment) {
			t.Fatalf("Redis handover script is missing %q", fragment)
		}
	}
}
