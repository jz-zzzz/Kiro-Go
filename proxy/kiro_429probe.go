package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	kiro429ProbeTTL        = time.Hour
	kiro429ProbeMaxEntries = 2000
)

type Kiro429ProbeLog struct {
	At        int64  `json:"at"`
	Endpoint  string `json:"endpoint"`
	Class     string `json:"class"`
	AccountID string `json:"accountId"`
	Email     string `json:"email"`
	Tier      string `json:"tier"`
	Body      string `json:"body"`
}

var (
	kiro429ProbeMu     sync.Mutex
	kiro429ProbeLoaded bool
	kiro429ProbeLogs   []Kiro429ProbeLog
)

func kiro429ProbeStorePath() string {
	return filepath.Join(config.GetConfigDir(), "kiro_429_probes.json")
}

func loadKiro429ProbeLogsLocked(now time.Time) {
	if kiro429ProbeLoaded {
		return
	}
	kiro429ProbeLoaded = true
	data, err := os.ReadFile(kiro429ProbeStorePath())
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warnf("[Kiro429Probe] Failed to load persisted logs: %v", err)
		}
		return
	}
	var logs []Kiro429ProbeLog
	if err := json.Unmarshal(data, &logs); err != nil {
		logger.Warnf("[Kiro429Probe] Failed to parse persisted logs: %v", err)
		return
	}
	kiro429ProbeLogs = logs
	if pruneKiro429ProbeLogsLocked(now) {
		saveKiro429ProbeLogsLocked()
	}
}

func saveKiro429ProbeLogsLocked() {
	path := kiro429ProbeStorePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		logger.Warnf("[Kiro429Probe] Failed to create log dir: %v", err)
		return
	}
	logs := kiro429ProbeLogs
	if logs == nil {
		logs = []Kiro429ProbeLog{}
	}
	data, err := json.MarshalIndent(logs, "", "  ")
	if err != nil {
		logger.Warnf("[Kiro429Probe] Failed to marshal logs: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		logger.Warnf("[Kiro429Probe] Failed to write logs: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logger.Warnf("[Kiro429Probe] Failed to replace logs: %v", err)
	}
}

func pruneKiro429ProbeLogsLocked(now time.Time) bool {
	before := len(kiro429ProbeLogs)
	cutoff := now.Add(-kiro429ProbeTTL).Unix()
	kept := kiro429ProbeLogs[:0]
	for _, entry := range kiro429ProbeLogs {
		if entry.At >= cutoff {
			kept = append(kept, entry)
		}
	}
	kiro429ProbeLogs = kept
	if extra := len(kiro429ProbeLogs) - kiro429ProbeMaxEntries; extra > 0 {
		copy(kiro429ProbeLogs, kiro429ProbeLogs[extra:])
		kiro429ProbeLogs = kiro429ProbeLogs[:len(kiro429ProbeLogs)-extra]
	}
	return len(kiro429ProbeLogs) != before
}

func recordKiro429ProbeLog(endpoint string, account *config.Account, body string) Kiro429ProbeLog {
	now := time.Now()
	entry := Kiro429ProbeLog{
		At:       now.Unix(),
		Endpoint: endpoint,
		Class:    classifyKiro429Body(body),
		Body:     compactLogBody(body, 4000),
	}
	if account != nil {
		entry.AccountID = account.ID
		entry.Email = account.Email
		entry.Tier = strings.TrimSpace(account.SubscriptionType + " " + account.SubscriptionTitle)
	}

	kiro429ProbeMu.Lock()
	loadKiro429ProbeLogsLocked(now)
	pruneKiro429ProbeLogsLocked(now)
	kiro429ProbeLogs = append(kiro429ProbeLogs, entry)
	pruneKiro429ProbeLogsLocked(now)
	saveKiro429ProbeLogsLocked()
	kiro429ProbeMu.Unlock()
	return entry
}

func getKiro429ProbeLogs() []Kiro429ProbeLog {
	now := time.Now()
	kiro429ProbeMu.Lock()
	defer kiro429ProbeMu.Unlock()
	loadKiro429ProbeLogsLocked(now)
	if pruneKiro429ProbeLogsLocked(now) {
		saveKiro429ProbeLogsLocked()
	}
	logs := make([]Kiro429ProbeLog, len(kiro429ProbeLogs))
	copy(logs, kiro429ProbeLogs)
	return logs
}

func clearKiro429ProbeLogs() int {
	kiro429ProbeMu.Lock()
	defer kiro429ProbeMu.Unlock()
	loadKiro429ProbeLogsLocked(time.Now())
	count := len(kiro429ProbeLogs)
	kiro429ProbeLogs = nil
	saveKiro429ProbeLogsLocked()
	return count
}

func getKiro429ProbeRate(accountID string) (int, float64) {
	if strings.TrimSpace(accountID) == "" {
		return 0, 0
	}
	logs := getKiro429ProbeLogs()
	count := 0
	for _, entry := range logs {
		if entry.AccountID == accountID {
			count++
		}
	}
	if count == 0 {
		return 0, 0
	}
	rate := float64(count) / 20.0
	if rate > 1 {
		rate = 1
	}
	return count, rate
}

func classifyKiro429Body(body string) string {
	lower := strings.ToLower(body)
	switch {
	case strings.TrimSpace(lower) == "":
		return "empty_429"
	case strings.Contains(lower, "suspicious activity") || strings.Contains(lower, "temporary limits") || strings.Contains(lower, "while we investigate"):
		return "suspicious_temporary_limits"
	case strings.Contains(lower, "too many requests") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "throttl") || strings.Contains(lower, "retry"):
		return "transient_rate_limit"
	case strings.Contains(lower, "quota") || strings.Contains(lower, "usage") || strings.Contains(lower, "exceeded") || strings.Contains(lower, "exhausted"):
		return "quota_or_usage"
	default:
		return "unknown_429"
	}
}

func compactLogBody(body string, limit int) string {
	body = strings.TrimSpace(body)
	body = strings.ReplaceAll(body, "\n", " ")
	body = strings.ReplaceAll(body, "\r", " ")
	body = strings.ReplaceAll(body, "\t", " ")
	for strings.Contains(body, "  ") {
		body = strings.ReplaceAll(body, "  ", " ")
	}
	if limit > 0 {
		runes := []rune(body)
		if len(runes) > limit {
			return string(runes[:limit]) + "..."
		}
	}
	return body
}
