// Package supervise keeps the router's background loops (health checks,
// discovery, cluster sync, metrics scraping, ...) alive: a panic in one is
// logged with its stack and the loop restarts after a backoff, instead of
// taking the whole router down with it.
package supervise

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Backoff bounds the wait before a loop that panicked is restarted; it
// doubles with each panic in a row, up to Max.
var Backoff = struct{ Min, Max time.Duration }{time.Second, time.Minute}

// Go runs fn in a goroutine until it returns or ctx is done, restarting it
// when it panics.
func Go(ctx context.Context, log *slog.Logger, name string, fn func(context.Context)) {
	go Run(ctx, log, name, fn)
}

// Run is Go without the goroutine.
func Run(ctx context.Context, log *slog.Logger, name string, fn func(context.Context)) {
	wait := Backoff.Min
	for {
		err := call(ctx, fn)
		if err == nil || ctx.Err() != nil {
			return
		}
		log.Error("background task panicked; restarting", "task", name, "error", err, "restart_in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, Backoff.Max)
	}
}

// call runs fn and turns a panic into an error carrying the stack.
func call(ctx context.Context, fn func(context.Context)) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("%v\n%s", v, debug.Stack())
		}
	}()
	fn(ctx)
	return nil
}
