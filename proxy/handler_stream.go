package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// sseHeartbeatInterval bounds how long the client-facing SSE connection may sit
// idle (no frame written) before we emit a `: ping` comment frame. Long upstream
// pauses — extended-thinking gaps, slow tool generation, or reasoning frames
// dropped when the client disabled thinking — otherwise leave the client→proxy
// socket silent long enough for intermediaries (NAT, reverse proxies, load
// balancers) to drop it, which surfaces to the user as a mid-stream cutoff.
const sseHeartbeatInterval = 10 * time.Second

// sseGuard wraps a streaming ResponseWriter so that (a) every SSE frame write is
// mutually exclusive with the heartbeat goroutine's pings — each frame is written
// by a single Write call (fmt.Fprintf / json.Encoder buffer the whole frame
// first), so holding the mutex per Write guarantees frames and pings never
// interleave byte-wise — and (b) an idle connection is kept warm with periodic
// comment frames.
//
// Heartbeats only start AFTER the first real frame is written (activated): the
// first frame commits the 200 status line, so pinging earlier would foreclose the
// handler's ability to fail over to another account and return a true error
// status. The pre-activation wait is bounded by the transport's
// ResponseHeaderTimeout instead.
type sseGuard struct {
	w       http.ResponseWriter
	flusher http.Flusher

	mu        sync.Mutex
	lastWrite time.Time
	activated bool
	closed    bool

	stopCh chan struct{}
	doneCh chan struct{}
}

func newSSEGuard(w http.ResponseWriter, flusher http.Flusher) *sseGuard {
	g := &sseGuard{
		w:       w,
		flusher: flusher,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	go g.heartbeatLoop()
	return g
}

// Header implements http.ResponseWriter.
func (g *sseGuard) Header() http.Header { return g.w.Header() }

// WriteHeader implements http.ResponseWriter.
func (g *sseGuard) WriteHeader(status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.w.WriteHeader(status)
}

// Write implements http.ResponseWriter. See sseGuard for why per-Write locking is
// sufficient to keep frames atomic relative to heartbeat pings.
func (g *sseGuard) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.activated = true
	g.lastWrite = time.Now()
	return g.w.Write(p)
}

// Flush implements http.Flusher.
func (g *sseGuard) Flush() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.flusher.Flush()
}

func (g *sseGuard) heartbeatLoop() {
	defer close(g.doneCh)
	// Tick more often than the interval so an idle gap is detected promptly
	// rather than up to a full interval late.
	ticker := time.NewTicker(sseHeartbeatInterval / 3)
	defer ticker.Stop()
	for {
		select {
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.mu.Lock()
			if g.activated && !g.closed && time.Since(g.lastWrite) >= sseHeartbeatInterval {
				// `:` opens an SSE comment line; clients ignore it, so it keeps
				// the socket warm without perturbing the parsed event stream.
				if _, err := g.w.Write([]byte(": ping\n\n")); err == nil {
					g.flusher.Flush()
					g.lastWrite = time.Now()
				}
			}
			g.mu.Unlock()
		}
	}
}

// stop terminates the heartbeat goroutine and waits for it to exit, guaranteeing
// no further writes to the underlying ResponseWriter once the handler returns.
// Idempotent.
func (g *sseGuard) stop() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	g.mu.Unlock()
	close(g.stopCh)
	<-g.doneCh
}

type thinkingStreamSource int

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}
