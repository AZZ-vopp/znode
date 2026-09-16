// Package relay runs UDP relays that prepend a PROXY protocol v2 header to
// every datagram.  It is intentionally independent of cobra so an Agent can
// own relays declared by the panel without spawning child processes.
package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
	log "github.com/sirupsen/logrus"
)

const (
	DefaultTTL      = 2 * time.Minute
	DefaultMaxFlows = 512
	maxDatagram     = 65535
)

type Config struct {
	ID, Network, Listen, Upstream string
	TTL                           time.Duration
	MaxFlows                      int
}

// Run is the foreground/manual form used by `znode udp-relay`.
func Run(ctx context.Context, cfg Config) error {
	r, err := start(ctx, cfg)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		r.close()
		return nil
	case err := <-r.done:
		return err
	}
}

func (c Config) Validate() error {
	if c.TTL < time.Second || c.TTL > 24*time.Hour {
		if c.TTL <= 0 {
			return errors.New("udp relay ttl must be greater than zero")
		}
		return errors.New("udp relay ttl must be between 1s and 24h")
	}
	if c.MaxFlows <= 0 {
		return errors.New("udp relay max-flows must be greater than zero")
	}
	if c.MaxFlows > 100000 {
		return errors.New("udp relay max-flows must be between 1 and 100000")
	}
	if c.Network != "udp4" && c.Network != "udp6" {
		return errors.New("udp relay network must be udp4 or udp6")
	}
	if c.ID == "" {
		return errors.New("udp relay id is required")
	}
	if _, err := net.ResolveUDPAddr(c.Network, c.Listen); err != nil {
		return fmt.Errorf("resolve udp relay listen address: %w", err)
	}
	if _, err := net.ResolveUDPAddr(c.Network, c.Upstream); err != nil {
		return fmt.Errorf("resolve udp relay upstream address: %w", err)
	}
	return nil
}

type flow struct {
	client      *net.UDPAddr
	upstream    *net.UDPConn
	destination *net.UDPAddr
	seen        time.Time
}
type instance struct {
	cfg      Config
	conn     *net.UDPConn
	upstream *net.UDPAddr
	mu       sync.Mutex
	flows    map[string]*flow
	cancel   context.CancelFunc
	done     chan error
}

func start(parent context.Context, cfg Config) (*instance, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	listen, _ := net.ResolveUDPAddr(cfg.Network, cfg.Listen)
	upstream, _ := net.ResolveUDPAddr(cfg.Network, cfg.Upstream)
	conn, err := net.ListenUDP(cfg.Network, listen)
	if err != nil {
		return nil, fmt.Errorf("listen udp relay: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	r := &instance{cfg: cfg, conn: conn, upstream: upstream, flows: map[string]*flow{}, cancel: cancel, done: make(chan error, 1)}
	go func() {
		err := r.serve(ctx)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.WithError(err).WithField("relay", cfg.ID).Warn("UDP relay stopped")
		}
		r.done <- err
	}()
	log.WithFields(log.Fields{"relay": cfg.ID, "listen": conn.LocalAddr(), "upstream": upstream}).Info("UDP PROXY v2 relay started")
	return r, nil
}
func (r *instance) close() {
	r.cancel()
	_ = r.conn.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, f := range r.flows {
		delete(r.flows, k)
		_ = f.upstream.Close()
	}
}
func (r *instance) update(cfg Config) error {
	upstream, err := net.ResolveUDPAddr(cfg.Network, cfg.Upstream)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, flow := range r.flows {
		delete(r.flows, key)
		_ = flow.upstream.Close()
	}
	r.cfg, r.upstream = cfg, upstream
	return nil
}
func (r *instance) serve(ctx context.Context) error {
	r.mu.Lock()
	ttl := r.cfg.TTL
	r.mu.Unlock()
	tick := time.NewTicker(cleanup(ttl))
	defer tick.Stop()
	go func() {
		for {
			select {
			case <-tick.C:
				r.expire()
			case <-ctx.Done():
				return
			}
		}
	}()
	b := make([]byte, maxDatagram)
	for {
		n, client, err := r.conn.ReadFromUDP(b)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		f, err := r.forClient(client)
		if err != nil {
			continue
		}
		d, err := datagram(client, f.destination, b[:n])
		if err == nil {
			_, _ = f.upstream.Write(d)
		}
	}
}
func (r *instance) forClient(client *net.UDPAddr) (*flow, error) {
	k := client.String()
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if f := r.flows[k]; f != nil {
		f.seen = now
		return f, nil
	}
	r.expireLocked(now)
	if len(r.flows) >= r.cfg.MaxFlows {
		return nil, errors.New("UDP relay flow limit reached")
	}
	u, e := net.DialUDP(r.cfg.Network, nil, r.upstream)
	if e != nil {
		return nil, e
	}
	f := &flow{client: clone(client), upstream: u, destination: clone(r.upstream), seen: now}
	r.flows[k] = f
	go r.replies(k, f)
	return f, nil
}
func (r *instance) replies(k string, f *flow) {
	b := make([]byte, maxDatagram)
	for {
		n, e := f.upstream.Read(b)
		if e != nil {
			return
		}
		r.mu.Lock()
		current := r.flows[k]
		if current == f {
			f.seen = time.Now()
		}
		r.mu.Unlock()
		if current != f {
			return
		}
		_, _ = r.conn.WriteToUDP(b[:n], f.client)
	}
}
func (r *instance) expire() { r.mu.Lock(); defer r.mu.Unlock(); r.expireLocked(time.Now()) }
func (r *instance) expireLocked(now time.Time) {
	for k, f := range r.flows {
		if now.Sub(f.seen) >= r.cfg.TTL {
			delete(r.flows, k)
			_ = f.upstream.Close()
		}
	}
}
func datagram(client, upstream *net.UDPAddr, p []byte) ([]byte, error) {
	if client == nil || upstream == nil || client.IP == nil || upstream.IP == nil {
		return nil, errors.New("UDP relay requires concrete source and destination IP addresses")
	}
	h := proxyproto.HeaderProxyFromAddrs(2, client, upstream)
	if h.Command != proxyproto.PROXY || (h.TransportProtocol != proxyproto.UDPv4 && h.TransportProtocol != proxyproto.UDPv6) {
		return nil, errors.New("UDP relay source and upstream address families must match")
	}
	return h.FormatUDPDatagram(p)
}
func clone(a *net.UDPAddr) *net.UDPAddr {
	return &net.UDPAddr{IP: append(net.IP(nil), a.IP...), Port: a.Port, Zone: a.Zone}
}
func cleanup(ttl time.Duration) time.Duration {
	d := ttl / 2
	if d > time.Minute {
		d = time.Minute
	}
	if d <= 0 {
		d = time.Second
	}
	return d
}

// Supervisor atomically applies a desired set. A failed replacement leaves
// the old listener live, so a malformed panel update cannot drop users.
type Supervisor struct {
	mu     sync.Mutex
	ctx    context.Context
	relays map[string]*instance
}

func NewSupervisor(ctx context.Context) *Supervisor {
	return &Supervisor{ctx: ctx, relays: map[string]*instance{}}
}
func (s *Supervisor) Reconcile(configs []Config) error {
	next := map[string]Config{}
	for _, c := range configs {
		if _, ok := next[c.ID]; ok {
			return fmt.Errorf("duplicate udp relay id %q", c.ID)
		}
		if err := c.Validate(); err != nil {
			return err
		}
		next[c.ID] = c
		for id, other := range next {
			if id != c.ID && other.Network == c.Network && other.Listen == c.Listen {
				return fmt.Errorf("duplicate udp relay listen %s/%s", c.Network, c.Listen)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(next))
	for k := range next {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	started := map[string]*instance{}
	hot := map[string]Config{}
	for _, id := range keys {
		c := next[id]
		old := s.relays[id]
		if old != nil && old.cfg == c {
			continue
		}
		// Same socket updates are applied in place only after every new listener
		// has staged successfully, so an unrelated bad relay cannot interrupt it.
		if old != nil && old.cfg.Network == c.Network && old.cfg.Listen == c.Listen {
			if _, err := net.ResolveUDPAddr(c.Network, c.Upstream); err != nil {
				for _, v := range started {
					v.close()
				}
				return err
			}
			hot[id] = c
			continue
		}
		r, e := start(s.ctx, c)
		if e != nil {
			for _, v := range started {
				v.close()
			}
			return fmt.Errorf("start udp relay %s: %w", id, e)
		}
		started[id] = r
	}
	for id, config := range hot {
		if err := s.relays[id].update(config); err != nil {
			for _, v := range started {
				v.close()
			}
			return fmt.Errorf("update udp relay %s: %w", id, err)
		}
	}
	for id, old := range s.relays {
		if _, keep := next[id]; !keep || (started[id] != nil && old != nil && old != started[id]) {
			old.close()
		}
	}
	for id, r := range started {
		s.relays[id] = r
	}
	for id := range s.relays {
		if _, ok := next[id]; !ok {
			delete(s.relays, id)
		}
	}
	return nil
}
func (s *Supervisor) Close() { _ = s.Reconcile(nil) }
