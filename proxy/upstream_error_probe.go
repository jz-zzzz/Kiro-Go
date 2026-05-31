package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// isClientDisconnectError reports whether err stems from the client cancelling
// the request (context cancelled/deadline) or dropping the connection, rather
// than an upstream fault. Such failures must not be recorded as upstream error
// probes: they are caused by the caller, not the Kiro endpoint, and would
// otherwise flood the store with noise during normal client aborts.
func isClientDisconnectError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// io.EOF / ErrUnexpectedEOF mid-stream usually means the client closed the
	// connection; treat as a client disconnect to avoid false upstream blame.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "client disconnected") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "use of closed network connection")
}

// Upstream error probes capture non-429 upstream failures so operators can
// diagnose them after the fact. Unlike the live request ring (process-local,
// 200 entries) these are persisted with a bounded retention window, mirroring
// the 429 probe store. The two most useful-but-currently-undiagnosable cases:
//   - a non-200/non-429 response from the Kiro endpoint (e.g. HTTP 500/503),
//   - a mid-stream failure where the response started 200 but parseEventStream
//     returned an error partway through (surfaced to clients as "api_error").
const (
	upstreamErrorProbeTTL        = 6 * time.Hour
	upstreamErrorProbeMaxEntries = 2000
)

// UpstreamErrorProbeLog is a single recorded upstream failure.
type UpstreamErrorProbeLog struct {
	At         int64  `json:"at"`
	Endpoint   string `json:"endpoint"`
	Phase      string `json:"phase"`                // "response" | "stream" | "connect"
	StatusCode int    `json:"statusCode,omitempty"` // 0 for stream/connect errors
	Model      string `json:"model,omitempty"`
	AccountID  string `json:"accountId"`
	Email      string `json:"email"`
	Tier       string `json:"tier"`
	Body       string `json:"body"`
}

var (
	upstreamErrorProbeMu     sync.Mutex
	upstreamErrorProbeLoaded bool
	upstreamErrorProbeLogs   []UpstreamErrorProbeLog
)

func upstreamErrorProbeStorePath() string {
	return filepath.Join(config.GetConfigDir(), "upstream_error_probes.json")
}

func loadUpstreamErrorProbeLogsLocked(now time.Time) {
	if upstreamErrorProbeLoaded {
		return
	}
	upstreamErrorProbeLoaded = true
	data, err := os.ReadFile(upstreamErrorProbeStorePath())
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warnf("[UpstreamErrorProbe] Failed to load persisted logs: %v", err)
		}
		return
	}
	var logs []UpstreamErrorProbeLog
	if err := json.Unmarshal(data, &logs); err != nil {
		logger.Warnf("[UpstreamErrorProbe] Failed to parse persisted logs: %v", err)
		return
	}
	upstreamErrorProbeLogs = logs
	if pruneUpstreamErrorProbeLogsLocked(now) {
		saveUpstreamErrorProbeLogsLocked()
	}
}

func saveUpstreamErrorProbeLogsLocked() {
	path := upstreamErrorProbeStorePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		logger.Warnf("[UpstreamErrorProbe] Failed to create log dir: %v", err)
		return
	}
	logs := upstreamErrorProbeLogs
	if logs == nil {
		logs = []UpstreamErrorProbeLog{}
	}
	data, err := json.MarshalIndent(logs, "", "  ")
	if err != nil {
		logger.Warnf("[UpstreamErrorProbe] Failed to marshal logs: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		logger.Warnf("[UpstreamErrorProbe] Failed to write logs: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logger.Warnf("[UpstreamErrorProbe] Failed to replace logs: %v", err)
	}
}

func pruneUpstreamErrorProbeLogsLocked(now time.Time) bool {
	before := len(upstreamErrorProbeLogs)
	cutoff := now.Add(-upstreamErrorProbeTTL).Unix()
	kept := upstreamErrorProbeLogs[:0]
	for _, entry := range upstreamErrorProbeLogs {
		if entry.At >= cutoff {
			kept = append(kept, entry)
		}
	}
	upstreamErrorProbeLogs = kept
	if extra := len(upstreamErrorProbeLogs) - upstreamErrorProbeMaxEntries; extra > 0 {
		copy(upstreamErrorProbeLogs, upstreamErrorProbeLogs[extra:])
		upstreamErrorProbeLogs = upstreamErrorProbeLogs[:len(upstreamErrorProbeLogs)-extra]
	}
	return len(upstreamErrorProbeLogs) != before
}

// recordUpstreamErrorProbe persists one upstream failure and returns the stored
// entry. phase is one of "response", "stream", "connect".
func recordUpstreamErrorProbe(endpoint, phase string, statusCode int, model string, account *config.Account, body string) UpstreamErrorProbeLog {
	now := time.Now()
	entry := UpstreamErrorProbeLog{
		At:         now.Unix(),
		Endpoint:   endpoint,
		Phase:      phase,
		StatusCode: statusCode,
		Model:      model,
		Body:       compactLogBody(body, 4000),
	}
	if account != nil {
		entry.AccountID = account.ID
		entry.Email = account.Email
		entry.Tier = strings.TrimSpace(account.SubscriptionType + " " + account.SubscriptionTitle)
	}

	upstreamErrorProbeMu.Lock()
	loadUpstreamErrorProbeLogsLocked(now)
	pruneUpstreamErrorProbeLogsLocked(now)
	upstreamErrorProbeLogs = append(upstreamErrorProbeLogs, entry)
	pruneUpstreamErrorProbeLogsLocked(now)
	saveUpstreamErrorProbeLogsLocked()
	upstreamErrorProbeMu.Unlock()
	return entry
}

func getUpstreamErrorProbeLogs() []UpstreamErrorProbeLog {
	now := time.Now()
	upstreamErrorProbeMu.Lock()
	defer upstreamErrorProbeMu.Unlock()
	loadUpstreamErrorProbeLogsLocked(now)
	if pruneUpstreamErrorProbeLogsLocked(now) {
		saveUpstreamErrorProbeLogsLocked()
	}
	logs := make([]UpstreamErrorProbeLog, len(upstreamErrorProbeLogs))
	copy(logs, upstreamErrorProbeLogs)
	return logs
}

func clearUpstreamErrorProbeLogs() int {
	upstreamErrorProbeMu.Lock()
	defer upstreamErrorProbeMu.Unlock()
	loadUpstreamErrorProbeLogsLocked(time.Now())
	count := len(upstreamErrorProbeLogs)
	upstreamErrorProbeLogs = nil
	saveUpstreamErrorProbeLogsLocked()
	return count
}
