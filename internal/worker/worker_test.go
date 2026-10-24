package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoopRecoversAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		Loop(ctx, "test", time.Millisecond, func(context.Context) {
			if calls.Add(1) == 1 {
				panic("boom")
			}
		})
	}()
	deadline := time.After(5 * time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("loop stopped after a panic: %d calls", calls.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop")
	}
}
