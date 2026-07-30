package task

import (
	"context"
	"testing"
	"time"
)

func TestCloseWaitsForActiveExecutionToObserveCancellation(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	task := &Task{
		Name:     "lifecycle-test",
		Interval: time.Hour,
		Execute: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(finished)
			return ctx.Err()
		},
	}
	if err := task.Start(true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}

	closed := make(chan struct{})
	go func() {
		task.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("task close did not cancel the active execution")
	}
	select {
	case <-finished:
	default:
		t.Fatal("task close returned before the callback finished")
	}
}
