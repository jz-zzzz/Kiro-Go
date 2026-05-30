package proxy

import "testing"

func TestComputeTokensPerSec(t *testing.T) {
	cases := []struct {
		out     int
		latency int64
		want    float64
	}{
		{0, 1000, 0},     // no output tokens
		{100, 0, 0},      // no latency
		{100, 1000, 100}, // 100 tokens in 1s
		{50, 2000, 25},   // 50 tokens in 2s
		{3, 1000, 3},     // small
		{100, -5, 0},     // negative latency guarded
	}
	for _, c := range cases {
		if got := computeTokensPerSec(c.out, c.latency); got != c.want {
			t.Fatalf("computeTokensPerSec(%d,%d)=%v want %v", c.out, c.latency, got, c.want)
		}
	}
}

func TestLiveRequestStoreRecentNewestFirst(t *testing.T) {
	s := &liveRequestStore{buf: make([]LiveRequestRecord, 4)}

	for i := 1; i <= 3; i++ {
		s.add(LiveRequestRecord{Timestamp: int64(i), Model: "m", OutputTokens: i})
	}

	got := s.recent(10)
	if len(got) != 3 {
		t.Fatalf("expected 3 records, got %d", len(got))
	}
	// Newest first: timestamps 3, 2, 1.
	if got[0].Timestamp != 3 || got[1].Timestamp != 2 || got[2].Timestamp != 1 {
		t.Fatalf("expected newest-first order, got %d,%d,%d", got[0].Timestamp, got[1].Timestamp, got[2].Timestamp)
	}
	if s.total() != 3 {
		t.Fatalf("expected total 3, got %d", s.total())
	}
}

func TestLiveRequestStoreRingWrap(t *testing.T) {
	cap := 4
	s := &liveRequestStore{buf: make([]LiveRequestRecord, cap)}

	// Write 6 records into a 4-slot ring; only the last 4 survive (3..6).
	for i := 1; i <= 6; i++ {
		s.add(LiveRequestRecord{Timestamp: int64(i)})
	}

	got := s.recent(10)
	if len(got) != cap {
		t.Fatalf("expected %d records after wrap, got %d", cap, len(got))
	}
	// Newest first: 6, 5, 4, 3.
	wantOrder := []int64{6, 5, 4, 3}
	for i, w := range wantOrder {
		if got[i].Timestamp != w {
			t.Fatalf("record %d: got ts=%d want %d", i, got[i].Timestamp, w)
		}
	}
	if s.total() != 6 {
		t.Fatalf("expected total 6, got %d", s.total())
	}
}

func TestLiveRequestStoreRespectsLimit(t *testing.T) {
	s := &liveRequestStore{buf: make([]LiveRequestRecord, 10)}
	for i := 1; i <= 8; i++ {
		s.add(LiveRequestRecord{Timestamp: int64(i)})
	}
	got := s.recent(3)
	if len(got) != 3 {
		t.Fatalf("expected 3 records, got %d", len(got))
	}
	if got[0].Timestamp != 8 || got[2].Timestamp != 6 {
		t.Fatalf("expected ts 8..6, got %d..%d", got[0].Timestamp, got[2].Timestamp)
	}
}

func TestLiveRequestStoreEmpty(t *testing.T) {
	s := &liveRequestStore{buf: make([]LiveRequestRecord, 4)}
	got := s.recent(5)
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %d", len(got))
	}
}

func TestRecordLiveRequestDerivedFields(t *testing.T) {
	s := &liveRequestStore{buf: make([]LiveRequestRecord, 4)}
	// Use the store directly via add after applying the same derivations
	// recordLiveRequest performs, to validate the derivation helpers without
	// touching global state.
	rec := LiveRequestRecord{InputTokens: 10, OutputTokens: 20, LatencyMs: 2000}
	if rec.TotalTokens <= 0 {
		rec.TotalTokens = rec.InputTokens + rec.OutputTokens
	}
	rec.TokensPerSec = computeTokensPerSec(rec.OutputTokens, rec.LatencyMs)
	s.add(rec)

	got := s.recent(1)
	if len(got) != 1 {
		t.Fatal("expected 1 record")
	}
	if got[0].TotalTokens != 30 {
		t.Fatalf("expected total 30, got %d", got[0].TotalTokens)
	}
	if got[0].TokensPerSec != 10 {
		t.Fatalf("expected 10 tok/s, got %v", got[0].TokensPerSec)
	}
}
