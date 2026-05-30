package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	metricsRetention       = 30 * 24 * time.Hour
	metricsDefaultRange    = 24 * time.Hour
	metricsDefaultBucket   = 5 * time.Minute
	metricsFlushInterval   = 30 * time.Second
	metricsFileName        = "metrics_timeseries.json"
	metricsMaxResponseItem = 5000
)

type MetricsSample struct {
	Timestamp    int64   `json:"timestamp"`
	Protocol     string  `json:"protocol,omitempty"`
	Stream       bool    `json:"stream,omitempty"`
	Model        string  `json:"model,omitempty"`
	AccountID    string  `json:"accountId,omitempty"`
	AccountEmail string  `json:"accountEmail,omitempty"`
	Subscription string  `json:"subscription,omitempty"`
	APIKeyID     string  `json:"apiKeyId,omitempty"`
	APIKeyName   string  `json:"apiKeyName,omitempty"`
	Success      bool    `json:"success"`
	ErrorType    string  `json:"errorType,omitempty"`
	StatusCode   int     `json:"statusCode,omitempty"`
	InputTokens  int     `json:"inputTokens,omitempty"`
	OutputTokens int     `json:"outputTokens,omitempty"`
	TotalTokens  int     `json:"totalTokens,omitempty"`
	Credits      float64 `json:"credits,omitempty"`
	LatencyMs    int64   `json:"latencyMs,omitempty"`
	TTFTMs       int64   `json:"ttftMs,omitempty"`
}

type MetricsBucket struct {
	BucketStart int64  `json:"bucketStart"`
	Protocol    string `json:"protocol,omitempty"`
	Stream      bool   `json:"stream,omitempty"`
	Model       string `json:"model,omitempty"`
	AccountID   string `json:"accountId,omitempty"`
	APIKeyID    string `json:"apiKeyId,omitempty"`

	AccountEmail string `json:"accountEmail,omitempty"`
	Subscription string `json:"subscription,omitempty"`
	APIKeyName   string `json:"apiKeyName,omitempty"`

	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	InputTokens  int64   `json:"inputTokens,omitempty"`
	OutputTokens int64   `json:"outputTokens,omitempty"`
	TotalTokens  int64   `json:"totalTokens,omitempty"`
	Credits      float64 `json:"credits,omitempty"`

	LatencySumMs int64 `json:"latencySumMs,omitempty"`
	LatencyCount int64 `json:"latencyCount,omitempty"`
	TTFTSumMs    int64 `json:"ttftSumMs,omitempty"`
	TTFTCount    int64 `json:"ttftCount,omitempty"`

	Errors429    int64 `json:"errors429,omitempty"`
	QueueFull    int64 `json:"queueFull,omitempty"`
	QueueTimeout int64 `json:"queueTimeout,omitempty"`
}

type MetricsSummary struct {
	RangeSeconds int64   `json:"rangeSeconds"`
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	SuccessRate  float64 `json:"successRate"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	TotalTokens  int64   `json:"totalTokens"`
	Credits      float64 `json:"credits"`
	AvgLatencyMs int64   `json:"avgLatencyMs"`
	AvgTTFTMs    int64   `json:"avgTTFTMs"`
	Errors429    int64   `json:"errors429"`
	QueueFull    int64   `json:"queueFull"`
	QueueTimeout int64   `json:"queueTimeout"`
}

type MetricsPoint struct {
	T     int64   `json:"t"`
	Value float64 `json:"value"`
}

type MetricsSeriesResponse struct {
	RangeSeconds  int64          `json:"rangeSeconds"`
	BucketSeconds int64          `json:"bucketSeconds"`
	Metric        string         `json:"metric"`
	Points        []MetricsPoint `json:"points"`
}

type MetricsTopItem struct {
	Key          string  `json:"key"`
	Label        string  `json:"label"`
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	TotalTokens  int64   `json:"totalTokens"`
	Credits      float64 `json:"credits"`
	Errors429    int64   `json:"errors429"`
	QueueFull    int64   `json:"queueFull"`
	QueueTimeout int64   `json:"queueTimeout"`
	AvgLatencyMs int64   `json:"avgLatencyMs"`
	AvgTTFTMs    int64   `json:"avgTTFTMs"`
}

type metricsStore struct {
	mu      sync.RWMutex
	loaded  bool
	dirty   bool
	buckets map[string]*MetricsBucket
	stop    chan struct{}
}

var globalMetrics = &metricsStore{stop: make(chan struct{})}

func metricsPath() string {
	return filepath.Join(config.GetConfigDir(), metricsFileName)
}

func bucketMinute(ts int64) int64 {
	if ts <= 0 {
		ts = time.Now().Unix()
	}
	return ts - ts%60
}

func metricsKey(b MetricsBucket) string {
	return fmt.Sprintf("%d|%s|%t|%s|%s|%s", b.BucketStart, b.Protocol, b.Stream, b.Model, b.AccountID, b.APIKeyID)
}

func (s *metricsStore) ensureLoadedLocked() {
	if s.loaded {
		return
	}
	s.buckets = make(map[string]*MetricsBucket)
	path := metricsPath()
	data, err := os.ReadFile(path)
	if err == nil && len(data) > 0 {
		var buckets []MetricsBucket
		if err := json.Unmarshal(data, &buckets); err != nil {
			logger.Warnf("[Metrics] failed to parse %s: %v", path, err)
		} else {
			cutoff := time.Now().Add(-metricsRetention).Unix()
			for i := range buckets {
				b := buckets[i]
				if b.BucketStart < cutoff {
					continue
				}
				copyBucket := b
				s.buckets[metricsKey(copyBucket)] = &copyBucket
			}
		}
	} else if err != nil && !os.IsNotExist(err) {
		logger.Warnf("[Metrics] failed to read %s: %v", path, err)
	}
	s.loaded = true
	s.pruneLocked(time.Now())
}

func (s *metricsStore) saveLocked() error {
	if !s.loaded {
		return nil
	}
	path := metricsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	items := make([]MetricsBucket, 0, len(s.buckets))
	for _, b := range s.buckets {
		items = append(items, *b)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].BucketStart < items[j].BucketStart })
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func (s *metricsStore) pruneLocked(now time.Time) {
	cutoff := now.Add(-metricsRetention).Unix()
	for k, b := range s.buckets {
		if b.BucketStart < cutoff {
			delete(s.buckets, k)
			s.dirty = true
		}
	}
}

func recordMetricsSample(sample MetricsSample) {
	globalMetrics.Record(sample)
}

func (s *metricsStore) Record(sample MetricsSample) {
	if sample.Timestamp <= 0 {
		sample.Timestamp = time.Now().Unix()
	}
	if sample.TotalTokens <= 0 {
		sample.TotalTokens = sample.InputTokens + sample.OutputTokens
	}
	if sample.Protocol == "" {
		sample.Protocol = "unknown"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked()
	s.pruneLocked(time.Now())
	b := MetricsBucket{
		BucketStart: bucketMinute(sample.Timestamp),
		Protocol:    sample.Protocol,
		Stream:      sample.Stream,
		Model:       sample.Model,
		AccountID:   sample.AccountID,
		APIKeyID:    sample.APIKeyID,
	}
	key := metricsKey(b)
	cur := s.buckets[key]
	if cur == nil {
		cur = &b
		s.buckets[key] = cur
	}
	if sample.AccountEmail != "" {
		cur.AccountEmail = sample.AccountEmail
	}
	if sample.Subscription != "" {
		cur.Subscription = sample.Subscription
	}
	if sample.APIKeyName != "" {
		cur.APIKeyName = sample.APIKeyName
	}
	cur.Requests++
	if sample.Success {
		cur.Success++
	} else {
		cur.Failed++
	}
	cur.InputTokens += int64(sample.InputTokens)
	cur.OutputTokens += int64(sample.OutputTokens)
	cur.TotalTokens += int64(sample.TotalTokens)
	cur.Credits += sample.Credits
	if sample.LatencyMs > 0 {
		cur.LatencySumMs += sample.LatencyMs
		cur.LatencyCount++
	}
	if sample.TTFTMs > 0 {
		cur.TTFTSumMs += sample.TTFTMs
		cur.TTFTCount++
	}
	errType := strings.ToLower(sample.ErrorType)
	if sample.StatusCode == http.StatusTooManyRequests || strings.Contains(errType, "429") {
		cur.Errors429++
	}
	if strings.Contains(errType, "queue_full") || strings.Contains(errType, "queue full") {
		cur.QueueFull++
	}
	if strings.Contains(errType, "queue_timeout") || strings.Contains(errType, "queue timeout") {
		cur.QueueTimeout++
	}
	s.dirty = true
}

func startMetricsBackgroundFlush(stop <-chan struct{}) {
	globalMetrics.start(stop)
}

func (s *metricsStore) start(stop <-chan struct{}) {
	s.mu.Lock()
	s.ensureLoadedLocked()
	s.mu.Unlock()
	ticker := time.NewTicker(metricsFlushInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.Flush()
			case <-stop:
				s.Flush()
				return
			}
		}
	}()
}

func (s *metricsStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked()
	s.pruneLocked(time.Now())
	if !s.dirty {
		return
	}
	if err := s.saveLocked(); err != nil {
		logger.Warnf("[Metrics] failed to save metrics: %v", err)
	}
}

func resetMetricsStore() error {
	globalMetrics.mu.Lock()
	defer globalMetrics.mu.Unlock()
	globalMetrics.ensureLoadedLocked()
	globalMetrics.buckets = make(map[string]*MetricsBucket)
	globalMetrics.dirty = true
	return globalMetrics.saveLocked()
}

func (s *metricsStore) snapshot(rangeDur time.Duration) []MetricsBucket {
	if rangeDur <= 0 {
		rangeDur = metricsDefaultRange
	}
	now := time.Now()
	cutoff := now.Add(-rangeDur).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked()
	s.pruneLocked(now)
	items := make([]MetricsBucket, 0)
	for _, b := range s.buckets {
		if b.BucketStart >= cutoff {
			items = append(items, *b)
		}
	}
	return items
}

func summarizeMetrics(rangeDur time.Duration) MetricsSummary {
	items := globalMetrics.snapshot(rangeDur)
	out := MetricsSummary{RangeSeconds: int64(rangeDur.Seconds())}
	var latencySum, latencyCount, ttftSum, ttftCount int64
	for _, b := range items {
		out.Requests += b.Requests
		out.Success += b.Success
		out.Failed += b.Failed
		out.InputTokens += b.InputTokens
		out.OutputTokens += b.OutputTokens
		out.TotalTokens += b.TotalTokens
		out.Credits += b.Credits
		out.Errors429 += b.Errors429
		out.QueueFull += b.QueueFull
		out.QueueTimeout += b.QueueTimeout
		latencySum += b.LatencySumMs
		latencyCount += b.LatencyCount
		ttftSum += b.TTFTSumMs
		ttftCount += b.TTFTCount
	}
	if out.Requests > 0 {
		out.SuccessRate = float64(out.Success) / float64(out.Requests)
	}
	if latencyCount > 0 {
		out.AvgLatencyMs = latencySum / latencyCount
	}
	if ttftCount > 0 {
		out.AvgTTFTMs = ttftSum / ttftCount
	}
	return out
}

func metricsValue(b MetricsBucket, metric string) float64 {
	switch strings.ToLower(metric) {
	case "requests":
		return float64(b.Requests)
	case "success":
		return float64(b.Success)
	case "failed", "errors":
		return float64(b.Failed)
	case "inputtokens", "input_tokens", "input":
		return float64(b.InputTokens)
	case "outputtokens", "output_tokens", "output":
		return float64(b.OutputTokens)
	case "credits":
		return b.Credits
	case "latency", "latencyms":
		if b.LatencyCount == 0 {
			return 0
		}
		return float64(b.LatencySumMs) / float64(b.LatencyCount)
	case "ttft", "ttftms":
		if b.TTFTCount == 0 {
			return 0
		}
		return float64(b.TTFTSumMs) / float64(b.TTFTCount)
	case "429", "errors429":
		return float64(b.Errors429)
	case "queue":
		return float64(b.QueueFull + b.QueueTimeout)
	case "queuefull":
		return float64(b.QueueFull)
	case "queuetimeout":
		return float64(b.QueueTimeout)
	case "tokens", "totaltokens", "total_tokens", "":
		fallthrough
	default:
		return float64(b.TotalTokens)
	}
}

func buildMetricsTimeseries(rangeDur, bucketDur time.Duration, metric string) MetricsSeriesResponse {
	if rangeDur <= 0 {
		rangeDur = metricsDefaultRange
	}
	if bucketDur <= 0 {
		bucketDur = metricsDefaultBucket
	}
	items := globalMetrics.snapshot(rangeDur)
	now := time.Now().Unix()
	start := now - int64(rangeDur.Seconds())
	bucketSec := int64(bucketDur.Seconds())
	if bucketSec <= 0 {
		bucketSec = int64(metricsDefaultBucket.Seconds())
	}
	start = start - start%bucketSec
	values := make(map[int64]float64)
	counts := make(map[int64]float64)
	isAverage := strings.EqualFold(metric, "latency") || strings.EqualFold(metric, "latencyMs") || strings.EqualFold(metric, "ttft") || strings.EqualFold(metric, "ttftMs")
	for _, b := range items {
		t := b.BucketStart - b.BucketStart%bucketSec
		if isAverage {
			switch strings.ToLower(metric) {
			case "ttft", "ttftms":
				values[t] += float64(b.TTFTSumMs)
				counts[t] += float64(b.TTFTCount)
			default:
				values[t] += float64(b.LatencySumMs)
				counts[t] += float64(b.LatencyCount)
			}
		} else {
			values[t] += metricsValue(b, metric)
		}
	}
	points := make([]MetricsPoint, 0)
	limit := 0
	for t := start; t <= now; t += bucketSec {
		v := values[t]
		if isAverage && counts[t] > 0 {
			v = values[t] / counts[t]
		}
		points = append(points, MetricsPoint{T: t, Value: roundMetricValue(v)})
		limit++
		if limit >= metricsMaxResponseItem {
			break
		}
	}
	return MetricsSeriesResponse{RangeSeconds: int64(rangeDur.Seconds()), BucketSeconds: bucketSec, Metric: metric, Points: points}
}

func roundMetricValue(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

func buildMetricsTop(rangeDur time.Duration, groupBy, metric string, limit int) []MetricsTopItem {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	items := globalMetrics.snapshot(rangeDur)
	groups := make(map[string]*MetricsTopItem)
	var latencySum = make(map[string]int64)
	var latencyCount = make(map[string]int64)
	var ttftSum = make(map[string]int64)
	var ttftCount = make(map[string]int64)
	for _, b := range items {
		key, label := metricGroupKey(b, groupBy)
		if key == "" {
			key = "unknown"
		}
		g := groups[key]
		if g == nil {
			g = &MetricsTopItem{Key: key, Label: label}
			if g.Label == "" {
				g.Label = key
			}
			groups[key] = g
		}
		g.Requests += b.Requests
		g.Success += b.Success
		g.Failed += b.Failed
		g.InputTokens += b.InputTokens
		g.OutputTokens += b.OutputTokens
		g.TotalTokens += b.TotalTokens
		g.Credits += b.Credits
		g.Errors429 += b.Errors429
		g.QueueFull += b.QueueFull
		g.QueueTimeout += b.QueueTimeout
		latencySum[key] += b.LatencySumMs
		latencyCount[key] += b.LatencyCount
		ttftSum[key] += b.TTFTSumMs
		ttftCount[key] += b.TTFTCount
	}
	out := make([]MetricsTopItem, 0, len(groups))
	for key, g := range groups {
		if latencyCount[key] > 0 {
			g.AvgLatencyMs = latencySum[key] / latencyCount[key]
		}
		if ttftCount[key] > 0 {
			g.AvgTTFTMs = ttftSum[key] / ttftCount[key]
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		vi := topMetricValue(out[i], metric)
		vj := topMetricValue(out[j], metric)
		if vi == vj {
			return out[i].Label < out[j].Label
		}
		return vi > vj
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func metricGroupKey(b MetricsBucket, groupBy string) (string, string) {
	switch strings.ToLower(groupBy) {
	case "account", "accounts":
		label := b.AccountEmail
		if label == "" {
			label = b.AccountID
		}
		return b.AccountID, label
	case "apikey", "api_key", "key":
		label := b.APIKeyName
		if label == "" {
			label = b.APIKeyID
		}
		if label == "" {
			label = "anonymous"
		}
		return b.APIKeyID, label
	case "protocol":
		return b.Protocol, b.Protocol
	case "subscription", "tier":
		return b.Subscription, b.Subscription
	case "model", "models", "":
		fallthrough
	default:
		return b.Model, b.Model
	}
}

func topMetricValue(item MetricsTopItem, metric string) float64 {
	switch strings.ToLower(metric) {
	case "requests":
		return float64(item.Requests)
	case "success":
		return float64(item.Success)
	case "failed", "errors":
		return float64(item.Failed)
	case "inputtokens", "input_tokens", "input":
		return float64(item.InputTokens)
	case "outputtokens", "output_tokens", "output":
		return float64(item.OutputTokens)
	case "credits":
		return item.Credits
	case "429", "errors429":
		return float64(item.Errors429)
	case "queue":
		return float64(item.QueueFull + item.QueueTimeout)
	case "latency", "latencyms":
		return float64(item.AvgLatencyMs)
	case "ttft", "ttftms":
		return float64(item.AvgTTFTMs)
	case "tokens", "totaltokens", "total_tokens", "":
		fallthrough
	default:
		return float64(item.TotalTokens)
	}
}

func parseMetricsRange(raw string) time.Duration {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return metricsDefaultRange
	}
	if strings.HasSuffix(raw, "d") {
		n, _ := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if n > 0 {
			d := time.Duration(n) * 24 * time.Hour
			if d > metricsRetention {
				return metricsRetention
			}
			return d
		}
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		if d > metricsRetention {
			return metricsRetention
		}
		return d
	}
	return metricsDefaultRange
}

func parseMetricsBucket(raw string, rangeDur time.Duration) time.Duration {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	switch {
	case rangeDur <= time.Hour:
		return time.Minute
	case rangeDur <= 6*time.Hour:
		return 5 * time.Minute
	case rangeDur <= 24*time.Hour:
		return 15 * time.Minute
	case rangeDur <= 7*24*time.Hour:
		return time.Hour
	default:
		return 6 * time.Hour
	}
}

func recordRequestMetrics(protocol, model string, stream bool, account *config.Account, apiKeyID string, success bool, statusCode int, errorType string, inputTokens, outputTokens int, credits float64, startedAt time.Time) {
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	sample := MetricsSample{
		Timestamp:    time.Now().Unix(),
		Protocol:     protocol,
		Stream:       stream,
		Model:        model,
		APIKeyID:     apiKeyID,
		APIKeyName:   apiKeyMetricsName(apiKeyID),
		Success:      success,
		ErrorType:    errorType,
		StatusCode:   statusCode,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		Credits:      credits,
		LatencyMs:    time.Since(startedAt).Milliseconds(),
	}
	if account != nil {
		sample.AccountID = account.ID
		sample.AccountEmail = account.Email
		sample.Subscription = strings.TrimSpace(strings.TrimSpace(account.SubscriptionType + " " + account.SubscriptionTitle))
	}
	recordMetricsSample(sample)
}

func apiKeyMetricsName(apiKeyID string) string {
	if apiKeyID == "" {
		return ""
	}
	entry := config.GetApiKeyEntry(apiKeyID)
	if entry == nil {
		return ""
	}
	if entry.Name != "" {
		return entry.Name
	}
	return entry.ID
}

func metricsErrorDetails(err error, fallbackStatus int, fallbackType string) (int, string) {
	status := fallbackStatus
	errType := fallbackType
	if err == nil {
		return status, errType
	}
	if errors.Is(err, pool.ErrRoutingQueueFull) {
		return http.StatusTooManyRequests, "queue_full"
	}
	if errors.Is(err, pool.ErrRoutingQueueTimeout) {
		return http.StatusTooManyRequests, "queue_timeout"
	}
	var kiroErr *KiroAPIError
	if errors.As(err, &kiroErr) {
		status = kiroErr.StatusCode
		if status == http.StatusTooManyRequests {
			class := classifyKiro429Body(kiroErr.Body)
			if class != "" {
				errType = "kiro_429_" + class
			} else {
				errType = "kiro_429"
			}
		} else {
			errType = fmt.Sprintf("upstream_%d", status)
		}
		return status, errType
	}
	msg := err.Error()
	switch {
	case isTransient429ErrorMessage(msg):
		return http.StatusTooManyRequests, "kiro_429_transient"
	case isSuspicious429ErrorMessage(msg):
		return http.StatusTooManyRequests, "kiro_429_suspicious"
	case isQuotaErrorMessage(msg):
		return http.StatusTooManyRequests, "quota_429"
	case isAuthErrorMessage(msg):
		return http.StatusUnauthorized, "authentication"
	case isOverageErrorMessage(msg):
		return http.StatusPaymentRequired, "overage"
	}
	if errType == "" {
		errType = "api_error"
	}
	return status, errType
}
