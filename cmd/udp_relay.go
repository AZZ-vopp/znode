package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AZZ-vopp/znode/relay"
	"github.com/spf13/cobra"
)

const (
	defaultUDPRelayTTL      = 2 * time.Minute
	defaultUDPRelayMaxFlows = 512
)

type udpRelayConfig struct {
	Network, Listen, Upstream string
	TTL                       time.Duration
	MaxFlows                  int
}

var (
	udpRelayListen   string
	udpRelayUpstream string
	udpRelayNetwork  string
	udpRelayTTL      time.Duration
	udpRelayMaxFlows int
)
var udpRelayCommand = cobra.Command{Use: "udp-relay", Short: "Relay UDP with a PROXY protocol v2 header on every datagram", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
	return runUDPRelayCommand(udpRelayConfig{udpRelayNetwork, udpRelayListen, udpRelayUpstream, udpRelayTTL, udpRelayMaxFlows})
}}

func init() {
	udpRelayCommand.Flags().StringVar(&udpRelayListen, "listen", ":443", "UDP listen address")
	udpRelayCommand.Flags().StringVar(&udpRelayNetwork, "network", "udp4", "UDP address family: udp4 or udp6")
	udpRelayCommand.Flags().StringVar(&udpRelayUpstream, "upstream", "", "ZNode UDP address (host:port)")
	udpRelayCommand.Flags().DurationVar(&udpRelayTTL, "ttl", defaultUDPRelayTTL, "idle lifetime for a client UDP flow")
	udpRelayCommand.Flags().IntVar(&udpRelayMaxFlows, "max-flows", defaultUDPRelayMaxFlows, "maximum concurrent client UDP flows")
	_ = udpRelayCommand.MarkFlagRequired("upstream")
	command.AddCommand(&udpRelayCommand)
}
func runUDPRelayCommand(config udpRelayConfig) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runUDPRelay(ctx, config)
}
func runUDPRelay(ctx context.Context, config udpRelayConfig) error {
	return relay.Run(ctx, relay.Config{ID: "manual", Network: config.Network, Listen: config.Listen, Upstream: config.Upstream, TTL: config.TTL, MaxFlows: config.MaxFlows})
}
