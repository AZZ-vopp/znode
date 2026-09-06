package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/AZZ-vopp/znode/agent"
	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/conf"
	"github.com/AZZ-vopp/znode/core"
	"github.com/AZZ-vopp/znode/limiter"
	"github.com/AZZ-vopp/znode/node"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	config string
	watch  bool
)

var serverCommand = cobra.Command{
	Use:   "server",
	Short: "Run znode server",
	Run:   serverHandle,
	Args:  cobra.NoArgs,
}

func init() {
	serverCommand.PersistentFlags().
		StringVarP(&config, "config", "c",
			"/etc/znode/config.json", "config file path")
	serverCommand.PersistentFlags().
		BoolVarP(&watch, "watch", "w",
			true, "watch file path change")
	command.AddCommand(&serverCommand)
}

func serverHandle(_ *cobra.Command, _ []string) {
	showVersion()
	panel.SetClientVersion(version)
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: true,
		DisableQuote:     true,
		PadLevelText:     false,
	})
	if assetDirectory := configureAssetLocation(config); assetDirectory != "" {
		log.WithField("directory", assetDirectory).Info("Xray geodata loaded")
	} else {
		log.Warn("geoip.dat and geosite.dat were not found together; geoip:/geosite: routing rules may fail")
	}

	prepared, err := prepareInitialRuntime(config)
	if err != nil {
		log.WithField("err", err).Error("Prepare node runtime failed")
		return
	}
	if prepared.offline {
		log.WithFields(log.Fields{
			"err":   prepared.offlineCause,
			"nodes": len(prepared.config.NodeConfigs),
		}).Warn("Panel unavailable; starting last-known-good offline runtime")
	}
	applyLogConfig(prepared.config)

	if prepared.config.PprofPort != 0 {
		port := prepared.config.PprofPort
		go func() {
			log.Infof("Starting pprof server on :%d", port)
			if err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", port), nil); err != nil {
				log.WithField("err", err).Error("pprof server failed")
			}
		}()
	}

	limiter.Init()
	reloadCh := make(chan struct{}, 1)
	snapshotCh := make(chan struct{}, 1)
	fallbackCh := make(chan agent.FallbackUpdate, 1)
	running, err := startPreparedRuntime(prepared, reloadCh, snapshotCh)
	if err != nil {
		log.WithField("err", err).Error("Start runtime failed")
		return
	}
	defer func() {
		if err := shutdownRuntime(running, 30*time.Second); err != nil {
			log.WithField("err", err).Error("Terminal shutdown completed with incomplete accounting")
		}
	}()
	logRevokedAssignment(running.assignment)
	log.WithField("nodes", len(running.config.NodeConfigs)).Info("Nodes started")
	if err := persistRuntimeSnapshot(running.preparedRuntime); err != nil {
		log.WithField("err", err).Warn("Persist last-known-good runtime snapshot failed")
	}

	agentMonitor := agent.NewMonitor(reloadCh, fallbackCh)
	if err := agentMonitor.MarkApplied(running.config.AgentConfig, running.assignment); err != nil {
		log.WithField("err", err).Error("Start agent manifest monitor failed")
		return
	}
	agentMonitor.Start()
	defer agentMonitor.Close()

	if watch {
		// Do not let the watcher mutate the config used by the live core. The
		// reload path loads and validates a fresh snapshot first.
		watchConfig := conf.New()
		if err := watchConfig.Watch(config, func() {
			select {
			case reloadCh <- struct{}{}:
			default:
			}
		}); err != nil {
			log.WithField("err", err).Error("Start config watcher failed")
			return
		}
	}

	runtime.GC()
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(osSignals)

	for {
		select {
		case <-osSignals:
			log.Info("Shutdown signal received")
			return
		case <-reloadCh:
			log.Info("Reload signal received; reconciling assigned nodes")
			newRuntime, err := reloadRuntime(config, running, reloadCh)
			if err != nil {
				log.WithField("err", err).Error("Reload failed; keeping current runtime when possible")
				continue
			}
			running = newRuntime
			if err := persistRuntimeSnapshot(running.preparedRuntime); err != nil {
				log.WithField("err", err).Warn("Persist last-known-good runtime snapshot failed")
			}
			logRevokedAssignment(running.assignment)
			if err := agentMonitor.MarkApplied(running.config.AgentConfig, running.assignment); err != nil {
				log.WithField("err", err).Warn("Update agent manifest monitor failed")
			}
			log.WithField("nodes", len(running.config.NodeConfigs)).Info("Reload successful")
		case fallbackUpdate := <-fallbackCh:
			applyRuntimeFallbackConfig(running, fallbackUpdate)
			if err := persistRuntimeSnapshot(running.preparedRuntime); err != nil {
				log.WithField("err", err).Warn("Persist Redis fallback update failed")
			}
			log.Info("Applied Redis user fallback without reloading VPN inbounds")
		case <-snapshotCh:
			if err := persistRuntimeSnapshot(running.preparedRuntime); err != nil {
				log.WithField("err", err).Warn("Refresh last-known-good runtime snapshot failed")
			}
		}
	}
}

func applyRuntimeFallbackConfig(running *runningRuntime, update agent.FallbackUpdate) {
	if running == nil || running.preparedRuntime == nil {
		return
	}
	if running.nodes != nil {
		running.nodes.UpdateFallbackConfig(update.Config)
	}
	if running.config == nil {
		return
	}
	for index := range running.config.NodeConfigs {
		running.config.NodeConfigs[index].GlobalDeviceLimitConfig = cloneRuntimeFallbackConfig(update.Config)
	}
	running.assignment.FallbackRevision = update.Revision
	running.assignment.Revision = update.AggregateRevision
}

func cloneRuntimeFallbackConfig(source *conf.GlobalDeviceLimitConfig) *conf.GlobalDeviceLimitConfig {
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

func logRevokedAssignment(assignment agent.Assignment) {
	if assignment.AuthorizationRevoked {
		log.Warn("Agent authorization was revoked; all assigned logical nodes are stopped until valid credentials are installed")
	}
}

type preparedRuntime struct {
	config       *conf.Conf
	nodes        *node.Node
	assignment   agent.Assignment
	offline      bool
	offlineCause error
}

type runningRuntime struct {
	*preparedRuntime
	core          *core.V2Core
	terminalNodes nodeTerminator
	terminalCore  coreCloser
}

type nodeTerminator interface {
	BeginTerminalCoreOperations()
	Shutdown(context.Context) error
	CloseCoreOperations(context.Context) error
}

type coreCloser interface{ Close() error }

// runtimeStartError keeps replacement cleanup failures distinguishable from
// ordinary startup errors. Reload may roll back only after every resource owned
// by the failed replacement has been released; otherwise a rollback would
// create a second core/TUN on top of leaked resources.
type runtimeStartError struct {
	startErr   error
	cleanupErr error
}

func (e *runtimeStartError) Error() string {
	if e.cleanupErr == nil {
		return e.startErr.Error()
	}
	return fmt.Sprintf("%v; replacement cleanup: %v", e.startErr, e.cleanupErr)
}

func (e *runtimeStartError) Unwrap() error {
	return errors.Join(e.startErr, e.cleanupErr)
}

func prepareRuntime(configPath string) (*preparedRuntime, error) {
	newConf := conf.New()
	if err := newConf.LoadFromPath(configPath); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	assignment, err := agent.Resolve(ctx, newConf)
	if err != nil {
		return nil, fmt.Errorf("resolve agent assignment: %w", err)
	}
	newNodes, err := node.NewContext(ctx, newConf.NodeConfigs)
	if err != nil {
		return nil, err
	}
	if err := newNodes.Prepare(ctx, newConf.NodeConfigs); err != nil {
		return nil, err
	}
	return &preparedRuntime{config: newConf, nodes: newNodes, assignment: assignment}, nil
}

func startPreparedRuntime(prepared *preparedRuntime, reloadCh chan struct{}, snapshotChannels ...chan struct{}) (*runningRuntime, error) {
	newCore := core.New(prepared.config)
	newCore.ReloadCh = reloadCh
	if len(snapshotChannels) > 0 {
		newCore.SnapshotCh = snapshotChannels[0]
	}
	if err := newCore.Start(prepared.nodes.NodeInfos); err != nil {
		if cleanupErr := newCore.Close(); cleanupErr != nil {
			return nil, &runtimeStartError{startErr: err, cleanupErr: cleanupErr}
		}
		return nil, err
	}
	if err := prepared.nodes.Start(prepared.config.NodeConfigs, newCore); err != nil {
		cleanupErr := errors.Join(prepared.nodes.Close(), newCore.Close())
		if cleanupErr != nil {
			return nil, &runtimeStartError{startErr: err, cleanupErr: cleanupErr}
		}
		return nil, err
	}
	return &runningRuntime{preparedRuntime: prepared, core: newCore, terminalNodes: prepared.nodes, terminalCore: newCore}, nil
}

func reloadRuntime(configPath string, old *runningRuntime, reloadCh chan struct{}) (*runningRuntime, error) {
	// Manifest fetch, NodeInfo fetch and duplicate-port validation all happen
	// before the healthy old runtime is stopped.
	prepared, err := prepareRuntime(configPath)
	if err != nil {
		return nil, err
	}

	// Build all Xray configuration before stopping the healthy runtime, but do
	// not construct/start a core here: WireGuard construction creates TUNs and
	// observatory probes. A replacement starts only after the old runtime exits.
	if err := core.ValidateConfig(prepared.config, prepared.nodes.NodeInfos); err != nil {
		return nil, err
	}

	snapshotCh := old.core.SnapshotCh
	if err := old.nodes.Close(); err != nil {
		return nil, fmt.Errorf("close old nodes: %w", err)
	}
	if err := old.core.Close(); err != nil {
		// V2Core.Close intentionally keeps Server non-nil after an error. Do not
		// construct another core here: a failed close may have left WireGuard TUNs
		// and observatory workers alive. Restore controllers on the same core only.
		if restoreErr := restorePreviousControllers(old); restoreErr != nil {
			return nil, fmt.Errorf("close old core: %w; restore previous controllers: %v", err, restoreErr)
		}
		return nil, fmt.Errorf("close old core: %w; previous controllers restored", err)
	}
	newRuntime, err := startPreparedRuntime(prepared, reloadCh, snapshotCh)
	if err != nil {
		var startFailure *runtimeStartError
		if errors.As(err, &startFailure) && startFailure.cleanupErr != nil {
			return nil, fmt.Errorf("start replacement runtime: %w; previous runtime not restored because replacement cleanup failed", err)
		}
		if restoreErr := restorePreviousRuntime(old, reloadCh); restoreErr != nil {
			return nil, fmt.Errorf("start replacement runtime: %w; restore previous runtime: %v", err, restoreErr)
		}
		return nil, fmt.Errorf("start replacement runtime: %w; previous runtime restored", err)
	}

	applyLogConfig(prepared.config)
	runtime.GC()
	return newRuntime, nil
}

// restorePreviousControllers reuses the still-live old core after a failed
// core close. Starting a fresh core here could duplicate WireGuard TUNs when
// the old close returned before all features were actually released.
func restorePreviousControllers(old *runningRuntime) error {
	if old == nil || old.preparedRuntime == nil || old.nodes == nil || old.core == nil {
		return fmt.Errorf("previous runtime is unavailable")
	}
	return old.nodes.Start(old.config.NodeConfigs, old.core)
}

// restorePreviousRuntime is the final rollback barrier for errors that can
// only surface after the old listeners have been released (for example a
// port being claimed by another process between preflight and replacement).
// The caller keeps its original runningRuntime pointer, so update its core in
// place after the previous controllers have been started again.
func restorePreviousRuntime(old *runningRuntime, reloadCh chan struct{}) error {
	if old == nil || old.preparedRuntime == nil {
		return fmt.Errorf("previous runtime is unavailable")
	}
	restored, err := startPreparedRuntime(old.preparedRuntime, reloadCh, old.core.SnapshotCh)
	if err != nil {
		return err
	}
	old.core = restored.core
	old.terminalCore = restored.core
	return nil
}

func (r *runningRuntime) Close() error {
	if r == nil {
		return nil
	}
	if r.nodes != nil {
		if err := r.nodes.Close(); err != nil {
			// Node.Close restores controllers that were already stopped. Never
			// close the core while any final traffic capture is not durable.
			return err
		}
	}
	if r.core != nil {
		if err := r.core.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Shutdown closes a runtime for process termination. It always tries the core
// close after the bounded node accounting attempt and never invokes the
// transactional Close path that can restore listeners for reload.
func (r *runningRuntime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	var shutdownErr error
	terminator := r.terminalNodes
	if terminator == nil && r.nodes != nil {
		terminator = r.nodes
	}
	if terminator != nil {
		// Close admission for every controller before one slow terminal drain can
		// consume the shared deadline. Otherwise a later controller can begin a
		// raw-core operation after the runtime is already preparing to close it.
		terminator.BeginTerminalCoreOperations()
		shutdownErr = errors.Join(shutdownErr, terminator.Shutdown(ctx))
		shutdownErr = errors.Join(shutdownErr, terminator.CloseCoreOperations(ctx))
	}
	closer := r.terminalCore
	if closer == nil && r.core != nil {
		closer = r.core
	}
	if closer != nil {
		shutdownErr = errors.Join(shutdownErr, closer.Close())
	}
	return shutdownErr
}

func shutdownRuntime(r *runningRuntime, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.Shutdown(ctx)
}

func applyLogConfig(config *conf.Conf) {
	switch config.LogConfig.Level {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	}
	if config.LogConfig.Output == "" {
		return
	}
	f, err := os.OpenFile(config.LogConfig.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.WithField("err", err).Error("Open log file failed, using current output instead")
		return
	}
	oldWriter, oldIsFile := log.StandardLogger().Out.(*os.File)
	log.SetOutput(f)
	if oldIsFile && oldWriter != os.Stdout && oldWriter != os.Stderr && oldWriter != f {
		_ = oldWriter.Close()
	}
}
