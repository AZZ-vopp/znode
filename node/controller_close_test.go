package node

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/conf"
	vcore "github.com/AZZ-vopp/znode/core"
	"github.com/AZZ-vopp/znode/limiter"
)

func TestMultiNodeCloseRestoresEarlierControllersWhenLaterSpoolFails(t *testing.T) {
	spoolDirectory := withTemporaryTrafficSpool(t)
	panelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/server/UniProxy/user":
			_, _ = w.Write([]byte(`{"users":[]}`))
		case "/api/v1/server/UniProxy/alivelist":
			_, _ = w.Write([]byte(`{"alive":{}}`))
		default:
			_, _ = w.Write([]byte(`{"data":true}`))
		}
	}))
	defer panelServer.Close()

	controllers := make([]*Controller, 0, 2)
	infos := make([]*panel.NodeInfo, 0, 2)
	configs := make([]conf.NodeConfig, 0, 2)
	for id := 1; id <= 2; id++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		config := conf.NodeConfig{
			APIHost: panelServer.URL, NodeID: id, Key: "agent-token", AgentID: "agent-a",
		}
		client, err := panel.New(&config)
		if err != nil {
			t.Fatal(err)
		}
		info := &panel.NodeInfo{
			Id: id, Tag: "close-test-" + string(rune('0'+id)), Type: "vmess",
			PushInterval: time.Hour, PullInterval: time.Hour,
			Common: &panel.CommonNode{ListenIP: "127.0.0.1", ServerPort: port},
		}
		controller := NewController(client, &config, info)
		if err := controller.Prepare(context.Background()); err != nil {
			t.Fatal(err)
		}
		configs = append(configs, config)
		infos = append(infos, info)
		controllers = append(controllers, controller)
	}

	limiter.Init()
	core := vcore.New(conf.New())
	core.ReloadCh = make(chan struct{}, 1)
	if err := core.Start(infos); err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	nodes := &Node{controllers: controllers, NodeInfos: infos}
	if err := nodes.Start(configs, core); err != nil {
		t.Fatal(err)
	}

	// An empty traffic batch normally needs no file. Turn only the second
	// controller's target into a directory so its final durable commit fails
	// after the first controller has already closed successfully.
	blockedPath := controllers[1].trafficSpoolPath()
	if err := os.MkdirAll(blockedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := nodes.Close(); err == nil {
		t.Fatal("multi-node close unexpectedly ignored the second spool failure")
	}
	for index, controller := range controllers {
		if !controller.started || !controller.inboundActive || controller.closing {
			t.Fatalf("controller %d was not restored: started=%v inbound=%v closing=%v",
				index, controller.started, controller.inboundActive, controller.closing)
		}
		if _, err := core.GetUserManager(controller.tag); err != nil {
			t.Fatalf("controller %d inbound is unavailable after rollback: %v", index, err)
		}
	}

	if err := os.Remove(blockedPath); err != nil {
		t.Fatal(err)
	}
	if err := nodes.Close(); err != nil {
		t.Fatalf("retry after repairing the spool failed: %v", err)
	}
	for index, controller := range controllers {
		if controller.started || controller.inboundActive {
			t.Fatalf("controller %d remained active after a durable close", index)
		}
	}
	if _, err := os.Stat(spoolDirectory); err != nil {
		t.Fatalf("traffic spool directory disappeared: %v", err)
	}
}

func TestCloseControllersStartsEveryDrainConcurrently(t *testing.T) {
	controllers := []*Controller{{}, {}, {}}
	started := make(chan *Controller, len(controllers))
	release := make(chan struct{})
	done := make(chan []controllerCloseResult, 1)

	go func() {
		done <- closeControllersConcurrently(controllers, func(controller *Controller) error {
			started <- controller
			<-release
			return nil
		})
	}()

	seen := make(map[*Controller]bool, len(controllers))
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for len(seen) < len(controllers) {
		select {
		case controller := <-started:
			seen[controller] = true
		case <-deadline.C:
			t.Fatalf("only %d/%d controller drains started concurrently", len(seen), len(controllers))
		}
	}
	close(release)

	select {
	case results := <-done:
		if len(results) != len(controllers) {
			t.Fatalf("got %d close results, want %d", len(results), len(controllers))
		}
		for index, result := range results {
			if result.controller != controllers[index] || result.err != nil {
				t.Fatalf("unexpected result %d: %#v", index, result)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("parallel controller close did not finish after drains were released")
	}
}
