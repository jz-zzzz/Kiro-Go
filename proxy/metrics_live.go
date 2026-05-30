package proxy

import (
	"sync"
	"time"
)

// metricsLiveCapacity is the number of most-recent request records retained in
// memory for the live request stream. The buffer is a fixed-size ring; older
// entries are overwritten. This is process-local and not persisted.
const metricsLiveCapacity = 200

// LiveRequestRecord is a single completed request, surfaced in the live
// request stream of the concurrency dashboard.
type LiveRequestRecord struct {
	Timestamp    int64   `json:"timestamp"` // unix seconds (completion time)
	Protocol     string  `json:"protocol"`  // claude | openai | responses
	Model        string  `json:"model"`
	Stream       bool    `json:"stream"`
	Success      bool    `json:"success"`
	StatusCode   int     `json:"statusCode,omitempty"`
	ErrorType    string  `json:"errorType,omitempty"`
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	TotalTokens  int     `json:"totalTokens"`
	LatencyMs    int64   `json:"latencyMs"`
	TTFTMs       int64   `json:"ttftMs,omitempty"` // first-token latency; 0 when unknown
	TokensPerSec float64 `json:"tokensPerSec"`
	AccountID    string  `json:"accountId,omitempty"`
	AccountEmail string  `json:"accountEmail,omitempty"`
}

type liveRequestStore struct {
	mu    sync.RWMutex
	buf   []LiveRequestRecord
	next  int  // index where the next record will be written
	full  bool // whether the ring has wrapped at least once
	count uint64
}

var globalLiveRequests = &liveRequestStore{buf: make([]LiveRequestRecord, metricsLiveCapacity)}

func (s *liveRequestStore) add(rec LiveRequestRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf[s.next] = rec
	s.next = (s.next + 1) % len(s.buf)
	if s.next == 0 {
		s.full = true
	}
	s.count++
}

// recent returns up to limit most-recent records, newest first.
func (s *liveRequestStore) recent(limit int) []LiveRequestRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	size := s.next
	if s.full {
		size = len(s.buf)
	}
	if size == 0 {
		return []LiveRequestRecord{}
	}
	if limit <= 0 || limit > size {
		limit = size
	}

	out := make([]LiveRequestRecord, 0, limit)
	// Walk backwards from the most recently written slot.
	idx := s.next - 1
	if idx < 0 {
		idx += len(s.buf)
	}
	for i := 0; i < limit; i++ {
		out = append(out, s.buf[idx])
		idx--
		if idx < 0 {
			idx += len(s.buf)
		}
	}
	return out
}

func (s *liveRequestStore) total() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.count
}

// computeTokensPerSec returns output tokens per second given a latency in ms.
// Returns 0 when latency is non-positive or there are no output tokens.
func computeTokensPerSec(outputTokens int, latencyMs int64) float64 {
	if outputTokens <= 0 || latencyMs <= 0 {
		return 0
	}
	tps := float64(outputTokens) / (float64(latencyMs) / 1000.0)
	// Round to two decimals.
	return float64(int64(tps*100+0.5)) / 100
}

// recordLiveRequest appends a completed request to the live ring buffer.
func recordLiveRequest(rec LiveRequestRecord) {
	if rec.Timestamp <= 0 {
		rec.Timestamp = time.Now().Unix()
	}
	if rec.TotalTokens <= 0 {
		rec.TotalTokens = rec.InputTokens + rec.OutputTokens
	}
	if rec.TokensPerSec == 0 {
		rec.TokensPerSec = computeTokensPerSec(rec.OutputTokens, rec.LatencyMs)
	}
	globalLiveRequests.add(rec)
}

// recentLiveRequests returns the most-recent request records, newest first.
func recentLiveRequests(limit int) []LiveRequestRecord {
	return globalLiveRequests.recent(limit)
}
