package supervise

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestRestartsAfterPanicAndStopsWhenDone(t *testing.T) {
	Backoff.Min, Backoff.Max = time.Millisecond, 4*time.Millisecond
	var buf syncBuf
	log := slog.New(slog.NewTextHandler(&buf, nil))
	var runs atomic.Int32
	done := make(chan struct{})
	go func() {
		Run(context.Background(), log, "flaky", func(ctx context.Context) {
			if runs.Add(1) < 3 {
				panic("boom")
			}
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not finish")
	}
	if runs.Load() != 3 {
		t.Errorf("ran %d times, want 3 (two panics, then a clean return)", runs.Load())
	}
	if out := buf.String(); strings.Count(out, "background task panicked") != 2 || !strings.Contains(out, "boom") ||
		!strings.Contains(out, "supervise_test.go") {
		t.Errorf("log lacks the panics and their stacks:\n%s", out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		Run(ctx, log, "always", func(context.Context) { panic("again") })
		close(stopped)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("kept restarting after ctx was done")
	}
}
