package proxy

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// TestIsClientDisconnectError verifies client-initiated failures are filtered
// out so they never pollute the upstream error probe store.
func TestIsClientDisconnectError(t *testing.T) {
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"nil error", context.Background(), nil, false},
		{"cancelled ctx", cancelledCtx, errors.New("anything"), true},
		{"context.Canceled", context.Background(), context.Canceled, true},
		{"deadline exceeded", context.Background(), context.DeadlineExceeded, true},
		{"unexpected EOF", context.Background(), io.ErrUnexpectedEOF, true},
		{"reset by peer", context.Background(), errors.New("read: connection reset by peer"), true},
		{"broken pipe", context.Background(), errors.New("write: broken pipe"), true},
		{"real upstream error", context.Background(), errors.New("HTTP 500 from Kiro IDE: boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClientDisconnectError(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("isClientDisconnectError(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestUpstreamErrorProbePruneTTL ensures entries older than the retention window
// are dropped and the most-recent entries are retained under the capacity cap.
func TestUpstreamErrorProbePruneTTL(t *testing.T) {
	upstreamErrorProbeMu.Lock()
	defer upstreamErrorProbeMu.Unlock()

	// Restore global state after the test so we don't leak into other tests.
	savedLogs := upstreamErrorProbeLogs
	savedLoaded := upstreamErrorProbeLoaded
	defer func() {
		upstreamErrorProbeLogs = savedLogs
		upstreamErrorProbeLoaded = savedLoaded
	}()
	upstreamErrorProbeLoaded = true // skip disk load in test

	now := time.Now()
	upstreamErrorProbeLogs = []UpstreamErrorProbeLog{
		{At: now.Add(-7 * time.Hour).Unix(), Phase: "stream"}, // expired (>6h)
		{At: now.Add(-1 * time.Hour).Unix(), Phase: "stream"}, // kept
		{At: now.Unix(), Phase: "response"},                   // kept
	}

	changed := pruneUpstreamErrorProbeLogsLocked(now)
	if !changed {
		t.Fatalf("expected prune to drop the expired entry")
	}
	if len(upstreamErrorProbeLogs) != 2 {
		t.Fatalf("expected 2 entries after TTL prune, got %d", len(upstreamErrorProbeLogs))
	}
}
