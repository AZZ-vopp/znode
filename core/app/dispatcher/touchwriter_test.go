package dispatcher

import (
	"errors"
	"testing"

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
