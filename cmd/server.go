package cmd

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/AZZ-vopp/znode/agent"
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

	prepared, err := prepareRuntime(config)
	if err != nil {
		log.WithField("err", err).Error("Prepare node runtime failed")
		return
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
	running, err := startPreparedRuntime(prepared, reloadCh)
	if err != nil {
		log.WithField("err", err).Error("Start runtime failed")
		return
	}
	defer func() { running.Close() }()
	logRevokedAssignment(running.assignment)
	log.WithField("nodes", len(running.config.NodeConfigs)).Info("Nodes started")

	agentMonitor := agent.NewMonitor(reloadCh)
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
			logRevokedAssignment(running.assignment)
			if err := agentMonitor.MarkApplied(running.config.AgentConfig, running.assignment); err != nil {
				log.WithField("err", err).Warn("Update agent manifest monitor failed")
			}
			log.WithField("nodes", len(running.config.NodeConfigs)).Info("Reload successful")
		}
	}
}

func logRevokedAssignment(assignment agent.Assignment) {
	if assignment.AuthorizationRevoked {
		log.Warn("Agent authorization was revoked; all assigned logical nodes are stopped until valid credentials are installed")
	}
}

type preparedRuntime struct {
	config     *conf.Conf
	nodes      *node.Node
	assignment agent.Assignment
}

type runningRuntime struct {
	*preparedRuntime
	core *core.V2Core
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

func startPreparedRuntime(prepared *preparedRuntime, reloadCh chan struct{}) (*runningRuntime, error) {
	newCore := core.New(prepared.config)
	newCore.ReloadCh = reloadCh
	if err := newCore.Start(prepared.nodes.NodeInfos); err != nil {
		return nil, err
	}
	if err := prepared.nodes.Start(prepared.config.NodeConfigs, newCore); err != nil {
		_ = prepared.nodes.Close()
		_ = newCore.Close()
		return nil, err
	}
	return &runningRuntime{preparedRuntime: prepared, core: newCore}, nil
}

func reloadRuntime(configPath string, old *runningRuntime, reloadCh chan struct{}) (*runningRuntime, error) {
	// Manifest fetch, NodeInfo fetch and duplicate-port validation all happen
	// before the healthy old runtime is stopped.
	prepared, err := prepareRuntime(configPath)
	if err != nil {
		return nil, err
	}

	// The base core has no inbounds yet, so this is also safe to preflight while
	// old ports are still listening.
	newCore := core.New(prepared.config)
	newCore.ReloadCh = reloadCh
	if err := newCore.Start(prepared.nodes.NodeInfos); err != nil {
		return nil, err
	}

	if err := old.nodes.Close(); err != nil {
		_ = newCore.Close()
		return nil, fmt.Errorf("close old nodes: %w", err)
	}
	if err := old.core.Close(); err != nil {
		_ = newCore.Close()
		if restoreErr := restorePreviousRuntime(old, reloadCh); restoreErr != nil {
			return nil, fmt.Errorf("close old core: %w; restore previous runtime: %v", err, restoreErr)
		}
		return nil, fmt.Errorf("close old core: %w; previous runtime restored", err)
	}
	if err := prepared.nodes.Start(prepared.config.NodeConfigs, newCore); err != nil {
		_ = prepared.nodes.Close()
		_ = newCore.Close()
		if restoreErr := restorePreviousRuntime(old, reloadCh); restoreErr != nil {
			return nil, fmt.Errorf("start replacement nodes: %w; restore previous runtime: %v", err, restoreErr)
		}
		return nil, fmt.Errorf("start replacement nodes: %w; previous runtime restored", err)
	}

	applyLogConfig(prepared.config)
	runtime.GC()
	return &runningRuntime{preparedRuntime: prepared, core: newCore}, nil
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
	restored, err := startPreparedRuntime(old.preparedRuntime, reloadCh)
	if err != nil {
		return err
	}
	old.core = restored.core
	return nil
}

func (r *runningRuntime) Close() {
	if r == nil {
		return
	}
	if r.nodes != nil {
		_ = r.nodes.Close()
	}
	if r.core != nil {
		_ = r.core.Close()
	}
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
