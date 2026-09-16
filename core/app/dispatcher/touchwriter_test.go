package dispatcher

import (
	"errors"
	"testing"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	"github.com/AZZ-vopp/znode/common/format"
	"github.com/AZZ-vopp/znode/conf"
	"github.com/AZZ-vopp/znode/limiter"
	"github.com/xtls/xray-core/common/buf"
)

type touchWriterTestSink struct{ calls int }

func (w *touchWriterTestSink) WriteMultiBuffer(mb buf.MultiBuffer) error {
	w.calls++
	buf.ReleaseMulti(mb)
	return nil
}

func (*touchWriterTestSink) Close() error { return nil }

func TestDeviceTouchWriterReleasesRejectedBuffers(t *testing.T) {
	sink := &touchWriterTestSink{}
	writer := &deviceTouchWriter{
		writer: sink,
		touch:  func() bool { return false },
	}
	mb := buf.MultiBuffer{buf.FromBytes([]byte("blocked"))}
	err := writer.WriteMultiBuffer(mb)
	if !errors.Is(err, errDeviceHandoverRejected) {
		t.Fatalf("error = %v, want device handover rejection", err)
	}
	if sink.calls != 0 {
		t.Fatalf("rejected buffer reached downstream writer %d times", sink.calls)
	}
	if len(mb) != 1 || mb[0] != nil {
		t.Fatal("rejected multibuffer was not released")
	}
}

func TestPrepareDeviceTouchKeepsExactIPv6RelayOutOfTracker(t *testing.T) {
	limiter.Init()
	const tag = "dispatcher-relay-exclusion"
	const uuid = "relay-user"
	l := limiter.AddLimiter("vless", tag, []panel.UserInfo{{
		Id: 42, Uuid: uuid, DeviceLimit: 1,
	}}, nil, &conf.GlobalDeviceLimitConfig{}, "https://panel.example", []string{
		"2001:db8:1:2::",
	})
	defer limiter.DeleteLimiter(tag)
	key := format.UserTag(tag, uuid)

	if !prepareDeviceTouch(l, key, "2001:db8:1:2::")() {
		t.Fatal("exact relay IPv6 was rejected by the session touch path")
	}
	online, err := l.GetOnlineDevice()
	if err != nil {
		t.Fatal(err)
	}
	if len(*online) != 0 {
		t.Fatalf("exact relay IPv6 leaked into online devices: %+v", *online)
	}

	if !prepareDeviceTouch(l, key, "2001:db8:1:2:abcd::1")() {
		t.Fatal("real client adjacent to relay IPv6 was rejected")
	}
	online, err = l.GetOnlineDevice()
	if err != nil {
		t.Fatal(err)
	}
	if len(*online) != 1 || (*online)[0].IP != "2001:db8:1:2::" {
		t.Fatalf("real client was not tracked by IPv6 /64: %+v", *online)
	}
}
