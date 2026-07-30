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
	PollInterval         time.Duration
	AuthorizationRevoked bool
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
		PollInterval:         manifest.EffectivePollInterval(config.AgentConfig.PollInterval),
		AuthorizationRevoked: manifest.AuthorizationRevoked,
	}, nil
}

type manifestFetcher interface {
	GetManifest(context.Context) (*panel.AgentManifest, error)
}

type fetcherFactory func(conf.AgentConfig) (manifestFetcher, error)

func defaultFetcherFactory(config conf.AgentConfig) (manifestFetcher, error) {
	return panel.NewAgentClient(config)
}

// Monitor polls only the small assignment manifest. It signals the existing
// reload loop when the panel revision differs from the last successfully
// applied revision. MarkApplied is intentionally separate so a failed reload
// is retried while the healthy old runtime keeps serving traffic.
type Monitor struct {
	mu              sync.RWMutex
	config          conf.AgentConfig
	fetcher         manifestFetcher
	factory         fetcherFactory
	appliedRevision string
	pollInterval    time.Duration
	generation      uint64
	reloadCh        chan<- struct{}
	wake            chan struct{}
	stop            chan struct{}
	done            chan struct{}
	startOnce       sync.Once
	closeOnce       sync.Once
}

func NewMonitor(reloadCh chan<- struct{}) *Monitor {
	return newMonitor(reloadCh, defaultFetcherFactory)
}

func newMonitor(reloadCh chan<- struct{}, factory fetcherFactory) *Monitor {
	return &Monitor{
		factory:  factory,
		reloadCh: reloadCh,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
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
	interval := manifest.EffectivePollInterval(fallback)

	m.mu.Lock()
	if generation != m.generation {
		m.mu.Unlock()
		return nil
	}
	m.pollInterval = interval
	changed := revision != m.appliedRevision
	m.mu.Unlock()

	if changed && m.reloadCh != nil {
		select {
		case m.reloadCh <- struct{}{}:
		default:
		}
	}
	return nil
}

func (m *Monitor) Close() {
	m.closeOnce.Do(func() {
		close(m.stop)
		m.startOnce.Do(func() { close(m.done) })
		<-m.done
	})
}
