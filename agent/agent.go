package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/conf"
	log "github.com/sirupsen/logrus"
)

// Assignment is a fetched, validated snapshot of the logical nodes assigned
// to this VPS agent.
type Assignment struct {
	Revision             string
	NodeRevision         string
	FallbackRevision     string
	PollInterval         time.Duration
	AuthorizationRevoked bool
}

type FallbackUpdate struct {
	Config            *conf.GlobalDeviceLimitConfig
	Revision          string
	AggregateRevision string
}

// Resolve replaces Nodes with the authoritative agent manifest when agent mode
// is enabled. In manual mode it intentionally does nothing.
func Resolve(ctx context.Context, config *conf.Conf) (Assignment, error) {
	if !config.AgentConfig.Enable {
		return Assignment{}, nil
	}
	client, err := panel.NewAgentClient(config.AgentConfig)
	if err != nil {
		return Assignment{}, err
	}
	manifest, err := client.GetManifest(ctx)
	if err != nil {
		return Assignment{}, err
	}
	if err := reconcileMaintenance(ctx, client, manifest.Maintenance); err != nil {
		return Assignment{}, fmt.Errorf("reconcile agent maintenance: %w", err)
	}
	if err := reconcileCertificate(ctx, client, manifest.CertificateRequest); err != nil {
		return Assignment{}, fmt.Errorf("reconcile agent certificate: %w", err)
	}
	config.NodeConfigs = manifest.NodeConfigs(config.AgentConfig)
	return Assignment{
		Revision:             manifest.EffectiveRevision(),
		NodeRevision:         manifest.EffectiveNodeRevision(),
		FallbackRevision:     manifest.EffectiveFallbackRevision(),
		PollInterval:         manifest.EffectivePollInterval(config.AgentConfig.PollInterval),
		AuthorizationRevoked: manifest.AuthorizationRevoked,
	}, nil
}

type manifestFetcher interface {
	GetManifest(context.Context) (*panel.AgentManifest, error)
}

// A panel deploy can briefly return 401/403 while PHP workers and route/config
// caches are being replaced. Never tear down healthy inbounds because of one
// transient denial; a genuinely revoked Agent remains denied and reaches this
// threshold on the following polls.
const authorizationRevocationThreshold = 3

type fetcherFactory func(conf.AgentConfig) (manifestFetcher, error)

func defaultFetcherFactory(config conf.AgentConfig) (manifestFetcher, error) {
	return panel.NewAgentClient(config)
}

// Monitor polls only the small assignment manifest. It signals the existing
// reload loop when the panel revision differs from the last successfully
// applied revision. MarkApplied is intentionally separate so a failed reload
// is retried while the healthy old runtime keeps serving traffic.
type Monitor struct {
	mu                      sync.RWMutex
	config                  conf.AgentConfig
	fetcher                 manifestFetcher
	factory                 fetcherFactory
	appliedRevision         string
	appliedNodeRevision     string
	appliedFallbackRevision string
	authorizationDenials    int
	pollInterval            time.Duration
	generation              uint64
	reloadCh                chan<- struct{}
	fallbackCh              chan<- FallbackUpdate
	wake                    chan struct{}
	stop                    chan struct{}
	done                    chan struct{}
	startOnce               sync.Once
	closeOnce               sync.Once
}

func NewMonitor(reloadCh chan<- struct{}, fallbackChannels ...chan<- FallbackUpdate) *Monitor {
	var fallbackCh chan<- FallbackUpdate
	if len(fallbackChannels) > 0 {
		fallbackCh = fallbackChannels[0]
	}
	return newMonitorWithFallback(reloadCh, fallbackCh, defaultFetcherFactory)
}

func newMonitor(reloadCh chan<- struct{}, factory fetcherFactory) *Monitor {
	return newMonitorWithFallback(reloadCh, nil, factory)
}

func newMonitorWithFallback(reloadCh chan<- struct{}, fallbackCh chan<- FallbackUpdate, factory fetcherFactory) *Monitor {
	return &Monitor{
		factory:    factory,
		reloadCh:   reloadCh,
		fallbackCh: fallbackCh,
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// MarkApplied changes the credentials and revision only after a runtime has
// started successfully.
func (m *Monitor) MarkApplied(config conf.AgentConfig, assignment Assignment) error {
	var fetcher manifestFetcher
	var err error
	if config.Enable {
		fetcher, err = m.factory(config)
		if err != nil {
			return fmt.Errorf("create agent manifest monitor: %w", err)
		}
	}
	pollInterval := assignment.PollInterval
	if pollInterval <= 0 {
		pollInterval = time.Duration(config.PollInterval) * time.Second
	}
	if pollInterval <= 0 {
		pollInterval = 15 * time.Second
	}

	m.mu.Lock()
	m.config = config
	m.fetcher = fetcher
	m.appliedRevision = assignment.Revision
	m.appliedNodeRevision = assignment.NodeRevision
	if m.appliedNodeRevision == "" {
		m.appliedNodeRevision = assignment.Revision
	}
	m.appliedFallbackRevision = assignment.FallbackRevision
	m.authorizationDenials = 0
	m.pollInterval = pollInterval
	m.generation++
	m.mu.Unlock()

	select {
	case m.wake <- struct{}{}:
	default:
	}
	return nil
}

func (m *Monitor) Start() {
	m.startOnce.Do(func() { go m.run() })
}

func (m *Monitor) run() {
	defer close(m.done)
	for {
		interval := m.currentInterval()
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
			ctx, cancel := context.WithTimeout(context.Background(), conf.DefaultNodeTimeout*time.Second)
			err := m.pollOnce(ctx)
			cancel()
			if err != nil {
				log.WithField("err", err).Warn("Poll agent assignment manifest failed; keeping current nodes")
			}
		case <-m.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-m.stop:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func (m *Monitor) currentInterval() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.config.Enable {
		return time.Minute
	}
	if m.pollInterval <= 0 {
		return 15 * time.Second
	}
	return m.pollInterval
}

func (m *Monitor) pollOnce(ctx context.Context) error {
	m.mu.RLock()
	if !m.config.Enable || m.fetcher == nil {
		m.mu.RUnlock()
		return nil
	}
	fetcher := m.fetcher
	generation := m.generation
	fallback := m.config.PollInterval
	m.mu.RUnlock()

	manifest, err := fetcher.GetManifest(ctx)
	if err != nil {
		return err
	}
	if reporter, ok := fetcher.(maintenanceReporter); ok {
		if err := reconcileMaintenance(ctx, reporter, manifest.Maintenance); err != nil {
			return err
		}
	}
	if reporter, ok := fetcher.(certificateReporter); ok {
		if err := reconcileCertificate(ctx, reporter, manifest.CertificateRequest); err != nil {
			return err
		}
	}
	revision := manifest.EffectiveRevision()
	nodeRevision := manifest.EffectiveNodeRevision()
	fallbackRevision := manifest.EffectiveFallbackRevision()
	interval := manifest.EffectivePollInterval(fallback)

	m.mu.Lock()
	if generation != m.generation {
		m.mu.Unlock()
		return nil
	}
	m.pollInterval = interval
	if manifest.AuthorizationRevoked {
		m.authorizationDenials++
		if m.authorizationDenials < authorizationRevocationThreshold {
			denials := m.authorizationDenials
			m.mu.Unlock()
			log.WithFields(log.Fields{
				"attempt":  denials,
				"required": authorizationRevocationThreshold,
			}).Warn("Agent authorization was temporarily denied; keeping current nodes")
			return nil
		}
	} else {
		m.authorizationDenials = 0
	}
	componentAware := manifest.NodeRevision != "" && manifest.FallbackRevision != ""
	legacyChanged := revision != m.appliedRevision
	nodeChanged := nodeRevision != m.appliedNodeRevision
	fallbackChanged := fallbackRevision != m.appliedFallbackRevision
	fallbackCh := m.fallbackCh
	m.mu.Unlock()

	if !componentAware {
		if legacyChanged && m.reloadCh != nil {
			select {
			case m.reloadCh <- struct{}{}:
			default:
			}
		}
		return nil
	}
	if nodeChanged && m.reloadCh != nil {
		select {
		case m.reloadCh <- struct{}{}:
		default:
		}
		return nil
	}
	if fallbackChanged && fallbackCh != nil && hotSwappableFallback(manifest.GlobalDeviceLimitConfig) {
		config := cloneFallbackConfig(manifest.GlobalDeviceLimitConfig)
		select {
		case fallbackCh <- FallbackUpdate{
			Config:            config,
			Revision:          fallbackRevision,
			AggregateRevision: revision,
		}:
			m.mu.Lock()
			if generation == m.generation && nodeRevision == m.appliedNodeRevision {
				m.appliedRevision = revision
				m.appliedFallbackRevision = fallbackRevision
			}
			m.mu.Unlock()
		default:
		}
		return nil
	}
	// Device-limiter changes need a complete controller reconciliation. The hot
	// path above is deliberately limited to Enable=false user-list fallback.
	if fallbackChanged && m.reloadCh != nil {
		select {
		case m.reloadCh <- struct{}{}:
		default:
		}
	}
	return nil
}

func hotSwappableFallback(config *conf.GlobalDeviceLimitConfig) bool {
	return config == nil || !config.Enable
}

func cloneFallbackConfig(source *conf.GlobalDeviceLimitConfig) *conf.GlobalDeviceLimitConfig {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.RedisSentinelAddrs = append([]string(nil), source.RedisSentinelAddrs...)
	if source.SyncEnabled != nil {
		enabled := *source.SyncEnabled
		cloned.SyncEnabled = &enabled
	}
	return &cloned
}

func (m *Monitor) Close() {
	m.closeOnce.Do(func() {
		close(m.stop)
		m.startOnce.Do(func() { close(m.done) })
		<-m.done
	})
}
