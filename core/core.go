package core

import (
	"fmt"
	"sync"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/conf"
	"github.com/AZZ-vopp/znode/core/app/dispatcher"
	_ "github.com/AZZ-vopp/znode/core/distro/all"
	log "github.com/sirupsen/logrus"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/routing"
	coreConf "github.com/xtls/xray-core/infra/conf"
	"google.golang.org/protobuf/proto"
)

type AddUsersParams struct {
	Tag   string
	Users []panel.UserInfo
	*panel.NodeInfo
}

type V2Core struct {
	Config     *conf.Conf
	ReloadCh   chan struct{}
	SnapshotCh chan struct{}
	access     sync.Mutex
	Server     *core.Instance
	users      *UserMap
	ihm        inbound.Manager
	ohm        outbound.Manager
	dispatcher *dispatcher.DefaultDispatcher
}

// RequestSnapshot coalesces last-known-good persistence requests. User sync
// may update several logical nodes at once; one whole-runtime snapshot after
// those changes is sufficient.
func (v *V2Core) RequestSnapshot() {
	if v == nil || v.SnapshotCh == nil {
		return
	}
	select {
	case v.SnapshotCh <- struct{}{}:
	default:
	}
}

type UserMap struct {
	uidMap   map[string]int
	quiesced map[string]struct{}
	mapLock  sync.RWMutex
}

func New(config *conf.Conf) *V2Core {
	core := &V2Core{
		Config: config,
		users: &UserMap{
			uidMap:   make(map[string]int),
			quiesced: make(map[string]struct{}),
		},
	}
	return core
}

func (v *V2Core) Start(infos []*panel.NodeInfo) error {
	v.access.Lock()
	defer v.access.Unlock()
	// These values are read by the dispatcher registration callback while the
	// Xray instance is being constructed. Keep the process-wide defaults scoped
	// to an actual runtime start; config validation must remain side-effect free.
	dispatcher.ConfigureUDPContentSniffing(v.Config.ConnectionConfig.DisableUDPContentSniffing)
	dispatcher.ConfigureSessionLimits(
		v.Config.ConnectionConfig.MaxConnectionsPerUser,
		v.Config.ConnectionConfig.MaxConnections,
	)
	server, err := getCore(v.Config, infos)
	if err != nil {
		return err
	}
	runtimeDispatcher := server.GetFeature(routing.DispatcherType()).(*dispatcher.DefaultDispatcher)
	runtimeDispatcher.ConfigureUDPContentSniffing(v.Config.ConnectionConfig.DisableUDPContentSniffing)
	udpChoices := make(map[string]*bool, len(infos))
	for _, info := range infos {
		if info != nil && info.Common != nil && info.Tag != "" {
			udpChoices[info.Tag] = info.Common.DisableUDPContentSniffing
		}
	}
	runtimeDispatcher.ConfigureUDPContentSniffingByInbound(udpChoices)
	v.Server = server
	if err := v.Server.Start(); err != nil {
		return err
	}
	v.ihm = v.Server.GetFeature(inbound.ManagerType()).(inbound.Manager)
	v.ohm = v.Server.GetFeature(outbound.ManagerType()).(outbound.Manager)
	v.dispatcher = v.Server.GetFeature(routing.DispatcherType()).(*dispatcher.DefaultDispatcher)
	return nil
}

// ValidateConfig builds the complete Xray configuration without constructing
// an instance. Reload uses this while an old runtime is live, so WireGuard
// outbounds cannot create a second TUN device or start observatory probes.
func ValidateConfig(c *conf.Conf, infos []*panel.NodeInfo) error {
	_, err := buildCoreConfig(c, infos)
	return err
}

func (v *V2Core) Close() error {
	v.access.Lock()
	defer v.access.Unlock()
	if v.Server == nil {
		return nil
	}
	// Keep every handle intact until Xray confirms that all features closed.
	// The caller can then retry a failed close without dereferencing a core that
	// this method prematurely marked as gone.
	if err := v.Server.Close(); err != nil {
		return err
	}
	v.Config = nil
	v.ihm = nil
	v.ohm = nil
	v.dispatcher = nil
	v.Server = nil
	return nil
}

func getCore(c *conf.Conf, infos []*panel.NodeInfo) (*core.Instance, error) {
	config, err := buildCoreConfig(c, infos)
	if err != nil {
		return nil, err
	}
	server, err := core.New(config)
	if err != nil {
		return nil, fmt.Errorf("create core instance: %w", err)
	}
	log.Info("Xray Core Version: ", core.Version())
	return server, nil
}

func buildCoreConfig(c *conf.Conf, infos []*panel.NodeInfo) (*core.Config, error) {
	// Log Config
	coreLogConfig := &coreConf.LogConfig{
		LogLevel:  c.LogConfig.Level,
		AccessLog: c.LogConfig.Access,
		ErrorLog:  c.LogConfig.Output,
	}
	// Custom config
	dnsConfig, outBoundConfig, routeConfig, observatoryConfig, err := GetCustomConfig(infos)
	if err != nil {
		return nil, fmt.Errorf("build custom config: %w", err)
	}
	// Inbound config
	var inBoundConfig []*core.InboundHandlerConfig

	// Policy config
	levelPolicyConfig := &coreConf.Policy{
		StatsUserUplink:   true,
		StatsUserDownlink: true,
		Handshake:         proto.Uint32(c.ConnectionConfig.Handshake),
		ConnectionIdle:    proto.Uint32(c.ConnectionConfig.ConnIdle),
		UplinkOnly:        proto.Uint32(c.ConnectionConfig.UplinkOnly),
		DownlinkOnly:      proto.Uint32(c.ConnectionConfig.DownlinkOnly),
		BufferSize:        proto.Int32(c.ConnectionConfig.BufferSize),
	}
	corePolicyConfig := &coreConf.PolicyConfig{}
	corePolicyConfig.Levels = map[uint32]*coreConf.Policy{0: levelPolicyConfig}
	policyConfig, _ := corePolicyConfig.Build()
	// Build Xray conf
	apps := []*serial.TypedMessage{
		serial.ToTypedMessage(coreLogConfig.Build()),
		serial.ToTypedMessage(&dispatcher.Config{}),
		serial.ToTypedMessage(&stats.Config{}),
		serial.ToTypedMessage(&proxyman.InboundConfig{}),
		serial.ToTypedMessage(&proxyman.OutboundConfig{}),
		serial.ToTypedMessage(policyConfig),
		serial.ToTypedMessage(dnsConfig),
		serial.ToTypedMessage(routeConfig),
	}
	if observatoryConfig != nil {
		// burst observatory is registered by the direct package import in
		// custom.go; it supplies real per-outbound probe status to balancers.
		apps = append(apps, serial.ToTypedMessage(observatoryConfig))
	}
	return &core.Config{
		App:      apps,
		Inbound:  inBoundConfig,
		Outbound: outBoundConfig,
	}, nil
}
