// Package worker runs the background loops of the server process.
package worker

import (
	"context"
	"log"
	"runtime/debug"
	"time"
)

// Loop calls fn right away and then every interval until ctx is canceled.
// A panic in fn is logged and the loop continues with the next interval.
// Loop returns after the call in progress has returned.
func Loop(ctx context.Context, name string, interval time.Duration, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		once(ctx, name, fn)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func once(ctx context.Context, name string, fn func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: panic: %v\n%s", name, r, debug.Stack())
		}
	}()
	fn(ctx)
}
