package node

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/conf"
	vcore "github.com/AZZ-vopp/znode/core"
	"github.com/AZZ-vopp/znode/limiter"
)

type deviceBypassLimiter struct{ bypass bool }

func (*deviceBypassLimiter) UpdateAliveList(map[int]int) {}
func (*deviceBypassLimiter) UpdateUser(string, []panel.UserInfo, []panel.UserInfo, []panel.UserInfo) {
}
func (*deviceBypassLimiter) GetOnlineDevice() (*[]panel.OnlineUser, error) { return nil, nil }
func (l *deviceBypassLimiter) SetDeviceLimitBypass(bypass bool) bool {
	changed := l.bypass != bypass
	l.bypass = bypass
	return changed
}

type nodeConfigTimeout struct{}

func (nodeConfigTimeout) Error() string   { return "node config timeout" }
func (nodeConfigTimeout) Timeout() bool   { return true }
func (nodeConfigTimeout) Temporary() bool { return true }

func TestControllerNodeConfigOutageThresholdAndRecovery(t *testing.T) {
	l := &deviceBypassLimiter{}
	c := &Controller{tag: "node-12", limiter: l}
	for i := 0; i < 2; i++ {
		c.recordNodeConfigFailure(nodeConfigTimeout{})
		if l.bypass {
			t.Fatalf("bypassed after %d availability errors, want threshold 3", i+1)
		}
	}
	c.recordNodeConfigFailure(nodeConfigTimeout{})
	if !l.bypass || !c.deviceLimitBypass.Load() {
		t.Fatal("three timeout errors did not bypass device/IP limiting")
	}
	c.nodeConfigFailures.Store(0)
	c.SetDeviceLimitBypass(false) // valid 200/304 recovery path
	if l.bypass || c.nodeConfigFailures.Load() != 0 {
		t.Fatal("successful node-config poll did not restore device/IP limiting")
	}
	c.recordNodeConfigFailure(errors.New("HTTP 401"))
	if l.bypass || c.nodeConfigFailures.Load() != 0 {
		t.Fatal("non-availability error changed recovered device/IP limiting")
	}
	for i := 0; i < 3; i++ {
		c.recordNodeConfigFailure(nodeConfigTimeout{})
	}
	if !l.bypass {
		t.Fatal("outage did not bypass device/IP limiting")
	}
	c.recordNodeConfigFailure(errors.New("malformed node config"))
	if !l.bypass || c.nodeConfigFailures.Load() != 0 {
		t.Fatal("malformed config incorrectly ended an outage bypass")
	}
}

func TestControllerCanceledNodeConfigMonitorDoesNotCountOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client, err := panel.New(&conf.NodeConfig{APIHost: "http://127.0.0.1:1", NodeID: 12, Key: "token", AgentID: "agent-a"})
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{apiClient: client}
	if err := c.nodeInfoMonitor(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled monitor error = %v, want context canceled", err)
	}
	if c.nodeConfigFailures.Load() != 0 || c.deviceLimitBypass.Load() {
		t.Fatal("canceled monitor changed device/IP limiting")
	}
}

func TestControllerPreparePreservesPanelAvailabilityError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	config := &conf.NodeConfig{
		APIHost: "http://" + address,
		NodeID:  12,
		Key:     "token",
		AgentID: "agent-a",
	}
	client, err := panel.New(config)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewController(client, config, &panel.NodeInfo{
		Id: 12, Tag: "node-12", Type: "vmess", Common: &panel.CommonNode{},
	})
	if err := controller.Prepare(context.Background()); err == nil || !panel.IsControlPlaneAvailabilityError(err) {
		t.Fatalf("Prepare error lost panel availability cause: %v", err)
	}
}

func TestControllerPrepareClassifiesUserAndAliveHTTPOutages(t *testing.T) {
	for _, failingPath := range []string{
		"/api/v1/server/UniProxy/user",
		"/api/v1/server/UniProxy/alivelist",
	} {
		t.Run(failingPath, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case failingPath:
					w.WriteHeader(http.StatusServiceUnavailable)
				case "/api/v1/server/UniProxy/revision":
					_, _ = w.Write([]byte(`{"revision":"0"}`))
				case "/api/v1/server/UniProxy/user":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"users":[]}`))
				default:
					t.Fatalf("unexpected panel path %s", request.URL.Path)
				}
			}))
			defer server.Close()

			config := &conf.NodeConfig{APIHost: server.URL, NodeID: 12, Key: "token", AgentID: "agent-a"}
			client, err := panel.New(config)
			if err != nil {
				t.Fatal(err)
			}
			controller := NewController(client, config, &panel.NodeInfo{
				Id: 12, Tag: "node-12", Type: "vmess", Common: &panel.CommonNode{},
			})
			err = controller.Prepare(context.Background())
			if err == nil || !panel.IsControlPlaneAvailabilityError(err) {
				t.Fatalf("Prepare did not preserve HTTP outage from %s: %v", failingPath, err)
			}
		})
	}
}

func TestControllerPrepareAllowsEmptyUserList(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/server/UniProxy/user":
			_, _ = w.Write([]byte(`{"users":[]}`))
		case "/api/v1/server/UniProxy/alivelist":
			_, _ = w.Write([]byte(`{"alive":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	nodeConfig := &conf.NodeConfig{APIHost: server.URL, NodeID: 12, Key: "agent-token", AgentID: "agent-a"}
	client, err := panel.New(nodeConfig)
	if err != nil {
		t.Fatal(err)
	}
	info := &panel.NodeInfo{
		Id:           12,
		Tag:          "node-12",
		Type:         "vmess",
		PushInterval: time.Hour,
		PullInterval: time.Hour,
		Common: &panel.CommonNode{
			ListenIP:   "127.0.0.1",
			ServerPort: port,
		},
	}
	controller := NewController(client, nodeConfig, info)
	if err := controller.Prepare(context.Background()); err != nil {
		t.Fatalf("empty user list should be valid: %v", err)
	}
	if !controller.prepared || controller.userList == nil || len(controller.userList) != 0 {
		t.Fatalf("controller did not preserve an intentional empty user list: %+v", controller.userList)
	}

	limiter.Init()
	core := vcore.New(conf.New())
	core.ReloadCh = make(chan struct{}, 1)
	if err := core.Start([]*panel.NodeInfo{info}); err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if err := controller.Start(core); err != nil {
		t.Fatalf("empty-user inbound should start and wait for user sync: %v", err)
	}
	if !controller.started {
		t.Fatal("empty-user controller did not start")
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
}
