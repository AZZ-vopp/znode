package limiter

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/common/format"
	"github.com/AZZ-vopp/znode/common/rate"
	"github.com/AZZ-vopp/znode/conf"
	log "github.com/sirupsen/logrus"
)

var limitLock sync.RWMutex
var limiter map[string]*Limiter

func Init() {
	limitLock.Lock()
	limiter = map[string]*Limiter{}
	limitLock.Unlock()
}

type Limiter struct {
	Nodetype      string
	SpeedLimit    int
	UserLimitInfo *sync.Map // key: tag|uuid, value: UserLimitInfo
	SpeedLimiter  *sync.Map // key: tag|uuid, value: *DynamicBucket
	deviceMu      sync.RWMutex
	devices       *deviceTracker
	remote        remoteDeviceStore
	remoteEnabled bool
	failClosed    bool
	closed        bool
	// excludedDeviceIPs contains canonical, exact relay IPs supplied by the
	// logical node. It is immutable after construction, so CheckLimit and
	// TouchDevice can read it without extending their data-path lock scope.
	excludedDeviceIPs map[string]struct{}
	lastRemoteErr     atomic.Int64
	// deviceLimitBypass is controlled by the node-config health monitor. It
	// deliberately covers only IP/device admission: authentication and the
	// speed bucket below continue to apply while the panel is unreachable.
	deviceLimitBypass atomic.Bool
}

type UserLimitInfo struct {
	UID               int
	SpeedLimit        int
	DeviceLimit       int
	DynamicSpeedLimit int
	ExpireTime        int64
}

func AddLimiter(nodetype string, tag string, users []panel.UserInfo, alive map[int]int, deviceConfig *conf.GlobalDeviceLimitConfig, namespace string, excludedIPs ...[]string) *Limiter {
	var deviceLimitExcludedIPs []string
	if len(excludedIPs) > 0 {
		deviceLimitExcludedIPs = excludedIPs[0]
	}
	// Every limiter key is a user credential (UUID/HWID), so all protocols can
	// safely apply the same short IP migration grace. This intentionally leaves
	// Enable=false when no config was supplied, so it never creates Redis.
	if deviceConfig == nil {
		deviceConfig = &conf.GlobalDeviceLimitConfig{}
	}
	if deviceConfig != nil {
		copyConfig := *deviceConfig
		// The config type lives in conf so it can be decoded without an import cycle.
		// Keep the same safe defaults when callers construct it directly in tests.
		applyDeviceDefaults(&copyConfig)
		// The limiter key is the per-user credential, not the transport. A
		// VLESS/Trojan/VMess client may also move between Wi-Fi and cellular.
		copyConfig.CredentialHandover = true
		deviceConfig = &copyConfig
	}

	l := &Limiter{
		Nodetype:          nodetype,
		UserLimitInfo:     new(sync.Map),
		SpeedLimiter:      new(sync.Map),
		devices:           newDeviceTracker(deviceConfig),
		excludedDeviceIPs: normalizeExcludedDeviceIPs(deviceLimitExcludedIPs),
	}
	l.devices.SetAliveList(alive)
	if deviceConfig != nil && deviceConfig.Enable {
		l.remoteEnabled = true
		l.failClosed = deviceConfig.FailClosed
		remote, err := newRedisDeviceStore(deviceConfig, namespace)
		if err != nil {
			if l.failClosed {
				log.WithError(err).Error("Redis device limiter unavailable; device-limited users fail closed")
			} else {
				log.WithError(err).Warn("Redis device limiter disabled; using bounded local device tracking")
			}
		} else {
			l.remote = remote
			if !l.failClosed {
				l.devices.startRemoteRefresh(2)
			}
		}
	}
	for i := range users {
		l.UserLimitInfo.Store(format.UserTag(tag, users[i].Uuid), UserLimitInfo{
			UID:         users[i].Id,
			SpeedLimit:  users[i].SpeedLimit,
			DeviceLimit: users[i].DeviceLimit,
		})
	}
	limitLock.Lock()
	limiter[tag] = l
	limitLock.Unlock()
	return l
}

func GetLimiter(tag string) (info *Limiter, err error) {
	limitLock.RLock()
	info, ok := limiter[tag]
	limitLock.RUnlock()
	if !ok {
		return nil, errors.New("not found")
	}
	return info, nil
}

func DeleteLimiter(tag string) {
	limitLock.Lock()
	l := limiter[tag]
	delete(limiter, tag)
	limitLock.Unlock()
	if l != nil {
		l.Close()
	}
}

func (l *Limiter) Close() {
	l.deviceMu.Lock()
	devices := l.devices
	remote := l.remote
	l.devices = nil
	l.remote = nil
	l.remoteEnabled = false
	l.closed = true
	l.deviceMu.Unlock()
	if devices != nil {
		devices.Close()
	}
	if remote != nil {
		_ = remote.Close()
	}
}

func (l *Limiter) UpdateAliveList(alive map[int]int) {
	// Keep the panel's last global count as a conservative fallback when Redis
	// is disabled. The tracker still owns current local IPs and never reads this
	// map without copying it under a lock.
	l.deviceMu.RLock()
	defer l.deviceMu.RUnlock()
	if l.devices != nil {
		l.devices.SetAliveList(alive)
	}
}

// ClearDeviceIPs forgets only runtime IP admission state. An empty credential
// hash list clears every credential for this panel; authorization and traffic
// counters are deliberately untouched.
func (l *Limiter) ClearDeviceIPs(tag string, credentialHashes []string) {
	l.deviceMu.RLock()
	defer l.deviceMu.RUnlock()
	if l.devices == nil {
		return
	}
	if len(credentialHashes) == 0 {
		l.devices.Clear()
		if store, ok := l.remote.(interface{ ClearAll(context.Context) error }); ok {
			if err := store.ClearAll(context.Background()); err != nil {
				log.WithError(err).Warn("Unable to clear all Redis device limiter state")
			}
		}
		return
	}
	keys := make([]string, 0, len(credentialHashes))
	seen := make(map[string]struct{}, len(credentialHashes))
	for _, digest := range credentialHashes {
		key, valid := format.UserTagFromCredentialDigest(tag, digest)
		if !valid {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
		l.devices.Delete(key)
	}
	if store, ok := l.remote.(interface {
		DeleteMany(context.Context, []string) error
	}); ok {
		if err := store.DeleteMany(context.Background(), keys); err != nil {
			log.WithError(err).Warn("Unable to clear Redis device limiter state")
		}
		return
	}
	for _, key := range keys {
		if l.remote != nil {
			_ = l.remote.Delete(context.Background(), key)
		}
	}
}

// UpdateDeviceConfig replaces the Redis/device admission backend without
// rebuilding Xray listeners. The bounded local device state is copied into the
// replacement tracker so a panel-side Redis edit cannot briefly admit an extra
// device or disconnect established sessions.
func (l *Limiter) UpdateDeviceConfig(config *conf.GlobalDeviceLimitConfig, namespace string) error {
	if l == nil {
		return nil
	}
	if config == nil {
		config = &conf.GlobalDeviceLimitConfig{}
	}
	copyConfig := *config
	copyConfig.RedisSentinelAddrs = append([]string(nil), config.RedisSentinelAddrs...)
	if config.SyncEnabled != nil {
		enabled := *config.SyncEnabled
		copyConfig.SyncEnabled = &enabled
	}
	applyDeviceDefaults(&copyConfig)
	copyConfig.CredentialHandover = true

	nextDevices := newDeviceTracker(&copyConfig)
	var nextRemote *redisDeviceStore
	var remoteErr error
	if copyConfig.Enable {
		nextRemote, remoteErr = newRedisDeviceStore(&copyConfig, namespace)
		if nextRemote != nil && !copyConfig.FailClosed {
			nextDevices.startRemoteRefresh(2)
		}
	}

	l.deviceMu.Lock()
	if l.closed {
		l.deviceMu.Unlock()
		nextDevices.Close()
		if nextRemote != nil {
			_ = nextRemote.Close()
		}
		return errors.New("device limiter is closed")
	}
	if l.devices != nil {
		nextDevices.copyRuntimeStateFrom(l.devices)
	}
	oldDevices := l.devices
	oldRemote := l.remote
	l.devices = nextDevices
	l.remote = nil
	if nextRemote != nil {
		l.remote = nextRemote
	}
	l.remoteEnabled = copyConfig.Enable
	l.failClosed = copyConfig.FailClosed
	l.deviceMu.Unlock()

	if oldDevices != nil {
		oldDevices.Close()
	}
	if oldRemote != nil {
		_ = oldRemote.Close()
	}
	return remoteErr
}

// SetDeviceLimitBypass temporarily disables only device/IP admission checks.
// It is safe to call while sessions are being accepted and does not rebuild
// Xray. The return value reports an actual state transition.
func (l *Limiter) SetDeviceLimitBypass(bypass bool) bool {
	for {
		current := l.deviceLimitBypass.Load()
		if current == bypass {
			return false
		}
		if l.deviceLimitBypass.CompareAndSwap(current, bypass) {
			return true
		}
	}
}

func (l *Limiter) DeviceLimitBypassed() bool {
	return l.deviceLimitBypass.Load()
}

func (l *Limiter) UpdateUser(tag string, added []panel.UserInfo, deleted []panel.UserInfo, modified []panel.UserInfo) {
	l.deviceMu.RLock()
	defer l.deviceMu.RUnlock()
	for i := range deleted {
		key := format.UserTag(tag, deleted[i].Uuid)
		l.UserLimitInfo.Delete(key)
		l.SpeedLimiter.Delete(key)
		if l.devices != nil {
			l.devices.Delete(key)
		}
		if l.remote != nil {
			_ = l.remote.Delete(context.Background(), key)
		}
	}
	for i := range modified {
		key := format.UserTag(tag, modified[i].Uuid)
		l.UserLimitInfo.Store(key, UserLimitInfo{
			UID:         modified[i].Id,
			SpeedLimit:  modified[i].SpeedLimit,
			DeviceLimit: modified[i].DeviceLimit,
		})
		limit := int64(determineSpeedLimit(l.SpeedLimit, modified[i].SpeedLimit)) * 1000000 / 8
		if limit > 0 {
			if v, ok := l.SpeedLimiter.Load(key); ok {
				v.(*rate.DynamicBucket).Update(limit)
			} else {
				l.SpeedLimiter.Store(key, rate.NewDynamicBucket(limit))
			}
		} else {
			l.SpeedLimiter.Delete(key)
		}
	}
	for i := range added {
		key := format.UserTag(tag, added[i].Uuid)
		l.UserLimitInfo.Store(key, UserLimitInfo{
			UID:         added[i].Id,
			SpeedLimit:  added[i].SpeedLimit,
			DeviceLimit: added[i].DeviceLimit,
		})
	}
}

func (l *Limiter) UpdateDynamicSpeedLimit(tag, uuid string, limit int, expire time.Time) error {
	key := format.UserTag(tag, uuid)
	for {
		v, ok := l.UserLimitInfo.Load(key)
		if !ok {
			return errors.New("not found")
		}
		old := v.(UserLimitInfo)
		updated := old
		updated.DynamicSpeedLimit = limit
		updated.ExpireTime = expire.Unix()
		if l.UserLimitInfo.CompareAndSwap(key, old, updated) {
			return nil
		}
	}
}

// CheckLimit applies the per-user speed limit and the device/IP limit. It is
// called for both TCP and UDP sessions; the old implementation skipped most
// UDP sources and therefore never enforced the configured limit for them.
func (l *Limiter) CheckLimit(ctx context.Context, taguuid string, ip string) (*rate.DynamicBucket, bool) {
	infoValue, ok := l.UserLimitInfo.Load(taguuid)
	if !ok {
		return nil, true
	}
	info := infoValue.(UserLimitInfo)
	now := time.Now()
	if info.ExpireTime != 0 && info.ExpireTime <= now.Unix() {
		if info.SpeedLimit != 0 {
			updated := info
			updated.DynamicSpeedLimit = 0
			updated.ExpireTime = 0
			l.UserLimitInfo.CompareAndSwap(taguuid, info, updated)
			info = updated
		} else {
			l.UserLimitInfo.Delete(taguuid)
			return nil, true
		}
	}

	if normalizedIP := normalizeIP(ip); normalizedIP != "" && !l.isDeviceLimitExcludedIP(ip) {
		l.deviceMu.RLock()
		devices := l.devices
		remote := l.remote
		remoteEnabled := l.remoteEnabled
		failClosed := l.failClosed
		// A panel outage may relax only the local/fail-open fallback policy.
		// Healthy Redis still has the authoritative cross-node admission state,
		// and FailClosed must never turn into fail-open merely because the panel
		// cannot currently refresh node configuration.
		enforceDevices := shouldEnforceDeviceLimit(l.deviceLimitBypass.Load(), remote != nil, remoteEnabled, failClosed)
		if !enforceDevices {
			l.deviceMu.RUnlock()
			goto speedLimit
		}
		if devices == nil {
			l.deviceMu.RUnlock()
			return nil, true
		}
		// FailClosed must also cover configuration/initialization failures. The
		// previous implementation enforced it only after a Redis client had been
		// created, so an unsupported network value silently bypassed the global
		// device limit even though the administrator explicitly requested denial.
		if info.DeviceLimit > 0 && remoteEnabled && failClosed && remote == nil {
			l.deviceMu.RUnlock()
			return nil, true
		}
		var store deviceStore
		if remote != nil {
			store = remote
		}
		allowed, err := devices.Observe(ctx, store, failClosed, taguuid, normalizedIP, info.UID, devices.effectiveLimit(info.DeviceLimit), now)
		l.deviceMu.RUnlock()
		if err != nil && l.shouldLogRemoteError(now) {
			log.WithError(err).Warn("Redis device limiter request failed; local bounded tracker is used")
		}
		if !allowed {
			return nil, true
		}
	}

speedLimit:
	limit := int64(determineSpeedLimit(l.SpeedLimit, determineSpeedLimit(info.SpeedLimit, info.DynamicSpeedLimit))) * 1000000 / 8
	if limit <= 0 {
		return nil, false
	}
	if v, ok := l.SpeedLimiter.Load(taguuid); ok {
		bucket := v.(*rate.DynamicBucket)
		return bucket, false
	}
	bucket := rate.NewDynamicBucket(limit)
	actual, loaded := l.SpeedLimiter.LoadOrStore(taguuid, bucket)
	if loaded {
		return actual.(*rate.DynamicBucket), false
	}
	return bucket, false
}

func shouldEnforceDeviceLimit(outageBypass, remoteAvailable, remoteEnabled, failClosed bool) bool {
	return !outageBypass || remoteAvailable || (remoteEnabled && failClosed)
}

// TouchDevice refreshes the bounded local entry and, when due, the Redis TTL.
// It is safe to call from the data path because same-IP touches are allocation
// free. Fail-open Redis refreshes are queued in bounded background workers;
// FailClosed keeps its synchronous enforcement semantics.
// TouchDevice refreshes a device lease and reports whether the current flow
// remains admitted. Hysteria2 uses this result to close a provisional
// Wi-Fi-to-mobile overlap if both addresses continue carrying traffic past the
// grace window.
func (l *Limiter) TouchDevice(taguuid, ip string) bool {
	value, ok := l.UserLimitInfo.Load(taguuid)
	if !ok {
		return false
	}
	if ip == "" {
		return true
	}
	if l.isDeviceLimitExcludedIP(ip) {
		return true
	}
	return l.touchPreparedDevice(taguuid, normalizeIP(ip), value.(UserLimitInfo))
}

// TouchPreparedDevice refreshes a session identity returned by PrepareDeviceIP.
// The caller has already checked the exact raw address for relay exclusion, so
// this path must not compare the /64-normalized identity with the exact list a
// second time.
func (l *Limiter) TouchPreparedDevice(taguuid, normalizedIP string) bool {
	value, ok := l.UserLimitInfo.Load(taguuid)
	if !ok {
		return false
	}
	return l.touchPreparedDevice(taguuid, normalizedIP, value.(UserLimitInfo))
}

func (l *Limiter) touchPreparedDevice(taguuid, normalizedIP string, info UserLimitInfo) bool {
	if normalizedIP == "" {
		return true
	}
	l.deviceMu.RLock()
	defer l.deviceMu.RUnlock()
	if !shouldEnforceDeviceLimit(l.deviceLimitBypass.Load(), l.remote != nil, l.remoteEnabled, l.failClosed) {
		return true
	}
	if l.devices == nil {
		return true
	}
	if info.DeviceLimit > 0 && l.remoteEnabled && l.failClosed && l.remote == nil {
		return false
	}
	var store deviceStore
	if l.remote != nil {
		store = l.remote
	}
	allowed, _ := l.devices.Observe(context.Background(), store, l.failClosed, taguuid, normalizedIP, info.UID, l.devices.effectiveLimit(info.DeviceLimit), time.Now())
	return allowed
}

func (l *Limiter) shouldLogRemoteError(now time.Time) bool {
	last := time.Unix(0, l.lastRemoteErr.Load())
	if now.Sub(last) < 30*time.Second {
		return false
	}
	return l.lastRemoteErr.CompareAndSwap(last.UnixNano(), now.UnixNano())
}

func (l *Limiter) GetOnlineDevice() (*[]panel.OnlineUser, error) {
	l.deviceMu.RLock()
	defer l.deviceMu.RUnlock()
	if l.devices == nil {
		empty := []panel.OnlineUser{}
		return &empty, nil
	}
	online, active := l.devices.Snapshot(time.Now())
	// Buckets for users that did not appear in the current TTL window are no
	// longer useful and are a common source of slow memory growth on long-lived
	// nodes.
	l.SpeedLimiter.Range(func(key, _ interface{}) bool {
		if _, ok := active[key.(string)]; !ok {
			l.SpeedLimiter.Delete(key)
		}
		return true
	})
	return &online, nil
}

func normalizeIP(raw string) string {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "::ffff:"))
	if raw == "" {
		return ""
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return ""
	}
	addr = addr.Unmap()
	// Mobile networks commonly rotate the IPv6 privacy/interface suffix while
	// keeping the delegated /64. Counting every suffix as a new device makes a
	// single phone intermittently lose Hysteria2/VLESS after reconnects. Keep
	// IPv4 exact, but use the conventional /64 network identity for globally
	// routable IPv6 addresses. The masked address is still a valid IP for the
	// existing online-user report contract.
	if addr.Is6() && addr.IsGlobalUnicast() {
		return netip.PrefixFrom(addr, 64).Masked().Addr().String()
	}
	return addr.String()
}

// normalizeExactIP returns the canonical identity for a configured relay IP.
// Unlike normalizeIP it intentionally does not collapse globally-routable
// IPv6 addresses to /64: exclusion is an exact allowlist, never a prefix.
func normalizeExactIP(raw string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return addr.Unmap().String()
}

func normalizeExcludedDeviceIPs(rawIPs []string) map[string]struct{} {
	if len(rawIPs) == 0 {
		return nil
	}
	excluded := make(map[string]struct{}, len(rawIPs))
	for _, raw := range rawIPs {
		if ip := normalizeExactIP(raw); ip != "" {
			excluded[ip] = struct{}{}
		}
	}
	return excluded
}

func (l *Limiter) isDeviceLimitExcludedIP(raw string) bool {
	if l == nil || len(l.excludedDeviceIPs) == 0 {
		return false
	}
	_, excluded := l.excludedDeviceIPs[normalizeExactIP(raw)]
	return excluded
}

// PrepareDeviceIP computes the tracker identity once per session while still
// matching relay exclusions against the exact source address. The tracker may
// group globally-routable IPv6 addresses by /64, but the exclusion allowlist
// must never broaden an exact relay address to that whole network.
func (l *Limiter) PrepareDeviceIP(raw string) (normalized string, excluded bool) {
	return normalizeIP(raw), l.isDeviceLimitExcludedIP(raw)
}

// NormalizeIP is exported for data-path wrappers so the address is parsed once
// per session instead of once per UDP packet.
func NormalizeIP(raw string) string {
	return normalizeIP(raw)
}

// deviceGracePtr returns a fresh pointer. applyDeviceDefaults is handed a
// shallow copy of the caller's config, so a default must replace the pointer
// rather than write through it, or it would mutate the original.
func deviceGracePtr(v int) *int {
	return &v
}

func applyDeviceDefaults(c *conf.GlobalDeviceLimitConfig) {
	if c.RedisNetwork == "" {
		c.RedisNetwork = "tcp"
	}
	if c.RedisAddr == "" {
		c.RedisAddr = "127.0.0.1:6379"
	}
	if c.Timeout <= 0 {
		c.Timeout = 1
	}
	if c.Expiry <= 0 {
		c.Expiry = 60
	}
	if c.Expiry < 10 {
		c.Expiry = 10
	}
	if c.RefreshInterval <= 0 || c.RefreshInterval >= c.Expiry {
		c.RefreshInterval = c.Expiry / 3
		if c.RefreshInterval < 5 {
			c.RefreshInterval = 5
		}
	}
	if c.RefreshInterval < 5 {
		c.RefreshInterval = 5
	}
	// Kept byte-for-byte in step with conf.GlobalDeviceLimitConfig.applyDefaults:
	// a file-loaded config and an agent hot-swapped one must admit identically.
	if c.HandoverGrace == nil {
		c.HandoverGrace = deviceGracePtr(15)
	}
	if *c.HandoverGrace < 0 {
		c.HandoverGrace = deviceGracePtr(0)
	}
	if *c.HandoverGrace > c.Expiry {
		c.HandoverGrace = deviceGracePtr(c.Expiry)
	}
	// An address that is still transmitting only refreshes its score once per
	// RefreshInterval, so it must get at least two chances to do so inside the
	// grace window. Otherwise a second client that is genuinely active looks
	// silent and gets evicted, which is the sharing case we must still refuse.
	if grace := *c.HandoverGrace; grace > 0 {
		if c.RefreshInterval > grace/2 {
			c.RefreshInterval = grace / 2
			if c.RefreshInterval < 5 {
				c.RefreshInterval = 5
			}
		}
		if c.RefreshInterval*2 > grace {
			c.HandoverGrace = deviceGracePtr(c.RefreshInterval * 2)
		}
	}
	if c.MaxIPsPerUser <= 0 {
		c.MaxIPsPerUser = 256
	}
	if c.MaxIPsPerUser > conf.MaximumIPsPerUser {
		c.MaxIPsPerUser = conf.MaximumIPsPerUser
	}
	if c.MaxIPsPerCredential <= 0 {
		c.MaxIPsPerCredential = 4
	}
	if c.MaxIPsPerCredential > conf.MaximumIPsPerCredential {
		c.MaxIPsPerCredential = conf.MaximumIPsPerCredential
	}
	if c.MaxIPsPerCredential > c.MaxIPsPerUser {
		c.MaxIPsPerCredential = c.MaxIPsPerUser
	}
	if c.KeyPrefix == "" {
		c.KeyPrefix = "znode:device"
	}
	if c.SyncChannel == "" {
		c.SyncChannel = "v2board:device-sync"
	}
	if c.SyncEnabled == nil {
		enabled := true
		c.SyncEnabled = &enabled
	}
}

type UserIpList struct {
	Uid    int      `json:"Uid"`
	IpList []string `json:"Ips"`
}
