package limiter

import (
	"context"
	"testing"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/common/format"
	"github.com/AZZ-vopp/znode/conf"
)

func TestNormalizeIP(t *testing.T) {
	if got := normalizeIP("::ffff:192.0.2.10"); got != "192.0.2.10" {
		t.Fatalf("mapped IPv4 normalized to %q", got)
	}
	if got := normalizeIP("2001:db8::1"); got != "2001:db8::1" {
		t.Fatalf("IPv6 normalized to %q", got)
	}
	if got := normalizeIP("not-an-ip"); got != "" {
		t.Fatalf("invalid IP normalized to %q", got)
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
