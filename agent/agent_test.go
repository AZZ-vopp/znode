package agent

import (
	"context"
	"testing"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/conf"
)

type fakeManifestFetcher struct {
	manifest *panel.AgentManifest
}

func (f *fakeManifestFetcher) GetManifest(context.Context) (*panel.AgentManifest, error) {
	return f.manifest, nil
}

func TestMonitorSignalsUntilRevisionIsApplied(t *testing.T) {
	reloadCh := make(chan struct{}, 1)
	fetcher := &fakeManifestFetcher{manifest: &panel.AgentManifest{Revision: "rev-2", Nodes: []int{1}}}
	monitor := newMonitor(reloadCh, func(conf.AgentConfig) (manifestFetcher, error) {
		return fetcher, nil
	})
	defer monitor.Close()

	config := conf.AgentConfig{Enable: true, APIHost: "https://panel.example", AgentID: "agent", AgentToken: "token", PollInterval: 15}
	if err := monitor.MarkApplied(config, Assignment{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	if err := monitor.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloadCh:
	default:
		t.Fatal("expected reload signal")
	}

	// A failed reload does not call MarkApplied, so the same desired revision
	// must trigger another attempt on the next poll.
	if err := monitor.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloadCh:
	default:
		t.Fatal("expected retry signal for unapplied revision")
	}

	if err := monitor.MarkApplied(config, Assignment{Revision: "rev-2"}); err != nil {
		t.Fatal(err)
	}
	if err := monitor.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloadCh:
		t.Fatal("unexpected signal for applied revision")
	default:
	}
}

func TestMonitorAppliesRedisFallbackWithoutReloadingNodes(t *testing.T) {
	reloadCh := make(chan struct{}, 1)
	fallbackCh := make(chan FallbackUpdate, 1)
	fetcher := &fakeManifestFetcher{manifest: &panel.AgentManifest{
		Revision:         "all-2",
		NodeRevision:     "nodes-1",
		FallbackRevision: "fallback-2",
		Nodes:            []int{1},
		GlobalDeviceLimitConfig: &conf.GlobalDeviceLimitConfig{
			RedisAddr: "redis.example:6379", UserFallbackEnabled: true,
		},
	}}
	monitor := newMonitorWithFallback(reloadCh, fallbackCh, func(conf.AgentConfig) (manifestFetcher, error) {
		return fetcher, nil
	})
	defer monitor.Close()

	config := conf.AgentConfig{Enable: true, APIHost: "https://panel.example", AgentID: "agent", AgentToken: "token"}
	if err := monitor.MarkApplied(config, Assignment{
		Revision: "all-1", NodeRevision: "nodes-1", FallbackRevision: "fallback-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := monitor.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-fallbackCh:
		if update.Config == nil || update.Config.RedisAddr != "redis.example:6379" || !update.Config.UserFallbackEnabled ||
			update.Revision != "fallback-2" || update.AggregateRevision != "all-2" {
			t.Fatalf("unexpected fallback update: %+v", update)
		}
	default:
		t.Fatal("expected a hot Redis fallback update")
	}
	select {
	case <-reloadCh:
		t.Fatal("Redis-only update reloaded VPN nodes")
	default:
	}

	if err := monitor.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fallbackCh:
		t.Fatal("already applied fallback was sent twice")
	default:
	}
}

func TestMonitorKeepsCurrentNodesUntilAuthorizationDenialIsPersistent(t *testing.T) {
	reloadCh := make(chan struct{}, 1)
	fetcher := &fakeManifestFetcher{manifest: &panel.AgentManifest{
		Revision:             "authorization-revoked:401",
		Nodes:                []int{},
		AuthorizationRevoked: true,
	}}
	monitor := newMonitor(reloadCh, func(conf.AgentConfig) (manifestFetcher, error) {
		return fetcher, nil
	})
	defer monitor.Close()

	config := conf.AgentConfig{Enable: true, APIHost: "https://panel.example", AgentID: "agent", AgentToken: "token", PollInterval: 15}
	if err := monitor.MarkApplied(config, Assignment{Revision: "healthy-revision"}); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt < authorizationRevocationThreshold; attempt++ {
		if err := monitor.pollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-reloadCh:
			t.Fatalf("transient authorization denial %d stopped healthy nodes", attempt)
		default:
		}
	}

	if err := monitor.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloadCh:
	default:
		t.Fatal("persistent authorization revocation did not trigger reconciliation")
	}
}

func TestMonitorResetsAuthorizationDenialsAfterAHealthyManifest(t *testing.T) {
	reloadCh := make(chan struct{}, 1)
	fetcher := &fakeManifestFetcher{manifest: &panel.AgentManifest{
		Revision:             "authorization-revoked:403",
		Nodes:                []int{},
		AuthorizationRevoked: true,
	}}
	monitor := newMonitor(reloadCh, func(conf.AgentConfig) (manifestFetcher, error) {
		return fetcher, nil
	})
	defer monitor.Close()

	config := conf.AgentConfig{Enable: true, APIHost: "https://panel.example", AgentID: "agent", AgentToken: "token", PollInterval: 15}
	if err := monitor.MarkApplied(config, Assignment{Revision: "healthy-revision"}); err != nil {
		t.Fatal(err)
	}
	if err := monitor.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	fetcher.manifest = &panel.AgentManifest{Revision: "healthy-revision", Nodes: []int{1}}
	if err := monitor.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	fetcher.manifest = &panel.AgentManifest{
		Revision:             "authorization-revoked:403",
		Nodes:                []int{},
		AuthorizationRevoked: true,
	}
	for attempt := 1; attempt < authorizationRevocationThreshold; attempt++ {
		if err := monitor.pollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-reloadCh:
			t.Fatalf("denial counter was not reset after a healthy response (attempt %d)", attempt)
		default:
		}
	}
}
