// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Endpoint configuration (auto-fallback on quota exhaustion).
type kiroEndpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

var kiroEndpoints = []kiroEndpoint{
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "",
		Name:      "Kiro IDE",
	},
	{
		URL:       "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		Name:      "CodeWhisperer",
	},
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		Name:      "AmazonQ",
	},
}

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// proxyClientCache caches http.Client instances keyed by proxy URL for per-account proxy support.
var proxyClientCache sync.Map

func init() {
	InitKiroHttpClient("")
}

// GetClientForProxy returns an http.Client configured for the given proxy URL.
// If proxyURL is empty, returns the global kiro HTTP client.
func GetClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroHttpStore.Load()
	}
	if cached, ok := proxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(proxyURL, client)
	return client
}

// GetRestClientForProxy returns a rest http.Client (30s timeout) for the given proxy URL.
// If proxyURL is empty, returns the global kiro REST HTTP client.
func GetRestClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroRestHttpStore.Load()
	}
	cacheKey := "rest:" + proxyURL
	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(cacheKey, client)
	return client
}

// ResolveAccountProxyURL returns the effective proxy URL for an account.
// Falls back to global config.GetProxyURL() if the account has no per-account proxy.
func ResolveAccountProxyURL(account *config.Account) string {
	if account != nil && account.ProxyURL != "" {
		return account.ProxyURL
	}
	return config.GetProxyURL()
}

// buildKiroTransport constructs an HTTP Transport with optional outbound proxy support.
func buildKiroTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			// Proxied connections cannot negotiate HTTP/2.
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

// InitKiroHttpClient initializes (or reinitializes) the HTTP clients used for Kiro API requests.
func InitKiroHttpClient(proxyURL string) {
	client := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroHttpStore.Store(client)

	restClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroRestHttpStore.Store(restClient)
}

// ==================== Request Structs ====================

// KiroPayload is the top-level request body sent to the Kiro API.
type KiroPayload struct {
	ConversationState struct {
		AgentContinuationId string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []KiroHistoryMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn      string           `json:"profileArn,omitempty"`
	InferenceConfig *InferenceConfig `json:"inferenceConfig,omitempty"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`

	// ToolSchemas maps names sent to Kiro back to the original client schema.
	// It lets the proxy coerce Kiro-generated tool inputs before returning them
	// to strict clients such as Claude Code.
	// Not serialized to the Kiro API request body.
	ToolSchemas map[string]interface{} `json:"-"`
}

type KiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []KiroImage              `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
}

type KiroToolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema InputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type InputSchema struct {
	JSON interface{} `json:"json"`
}

type KiroToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []KiroResultContent `json:"content"`
	Status    string              `json:"status"`
}

type KiroResultContent struct {
	Text string `json:"text"`
}

type KiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

type InferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	OnText         func(text string, isThinking bool)
	OnToolUse      func(toolUse KiroToolUse)
	OnComplete     func(inputTokens, outputTokens int)
	OnError        func(err error)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)
}

type KiroAPIError struct {
	StatusCode int
	Endpoint   string
	Body       string
}

func (e *KiroAPIError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Body) != "" {
		return fmt.Sprintf("HTTP %d from %s: %s", e.StatusCode, e.Endpoint, e.Body)
	}
	return fmt.Sprintf("HTTP %d from %s", e.StatusCode, e.Endpoint)
}

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

func IsKiroRateLimitError(err error) bool {
	if apiErr, ok := err.(*KiroAPIError); ok && apiErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if err == nil {
		return false
	}
	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "http 429") || strings.Contains(errMsg, "too many requests") || strings.Contains(errMsg, "quota") || strings.Contains(errMsg, "temporary limits") || strings.Contains(errMsg, "suspicious activity")
}

func IsKiroRetryableAccountError(err error) bool {
	if apiErr, ok := err.(*KiroAPIError); ok {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	if err == nil {
		return false
	}
	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "http 429") || strings.Contains(errMsg, "http 500") || strings.Contains(errMsg, "http 502") || strings.Contains(errMsg, "http 503") || strings.Contains(errMsg, "http 504") || strings.Contains(errMsg, "temporary limits") || strings.Contains(errMsg, "suspicious activity")
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}

	payload.ProfileArn = strings.TrimSpace(payload.ProfileArn)
	if account != nil {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			payload.ProfileArn = profileArn
		}
	}
}

// getSortedEndpoints returns endpoints ordered by user preference, with optional fallback.
func getSortedEndpoints(preferred string) []kiroEndpoint {
	fallback := config.GetEndpointFallback()

	var primary int
	switch preferred {
	case "kiro":
		primary = 0
	case "codewhisperer":
		primary = 1
	case "amazonq":
		primary = 2
	default:
		primary = 0 // "auto": Kiro endpoint remains the primary choice
	}

	if !fallback {
		// No fallback: only use the selected endpoint (or Kiro primary when preferred=auto)
		return []kiroEndpoint{kiroEndpoints[primary]}
	}

	// With fallback: selected first, then others in order
	result := []kiroEndpoint{kiroEndpoints[primary]}
	for i, ep := range kiroEndpoints {
		if i != primary {
			result = append(result, ep)
		}
	}
	return result
}

// CallKiroAPI calls the Kiro streaming API, trying each configured endpoint with automatic fallback.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	originalProfileArn := ""
	if payload != nil {
		originalProfileArn = payload.ProfileArn
		defer func() {
			payload.ProfileArn = originalProfileArn
		}()
	}
	setPayloadProfileArnForAccount(payload, account)

	if _, err := json.Marshal(payload); err != nil {
		return err
	}

	// Debug: dump full payload for troubleshooting upstream rejections
	if payloadJSON, err := json.Marshal(payload); err == nil {
		logger.Debugf("[KiroAPI] Request payload: %s", string(payloadJSON))
	}

	// Wrap OnToolUse to normalize Kiro-generated tool inputs and restore
	// original tool names for strict clients such as Claude Code.
	if callback != nil && callback.OnToolUse != nil && payload != nil && (len(payload.ToolNameMap) > 0 || len(payload.ToolSchemas) > 0) {
		originalOnToolUse := callback.OnToolUse
		nameMap := payload.ToolNameMap
		schemaMap := payload.ToolSchemas
		wrapped := *callback
		wrapped.OnToolUse = func(tu KiroToolUse) {
			if schema, ok := schemaMap[tu.Name]; ok {
				tu.Input = sanitizeToolInput(tu.Input, schema)
			} else {
				tu.Input = normalizeCommonToolInputScalars(tu.Input)
			}
			if original, ok := nameMap[tu.Name]; ok {
				tu.Name = original
			}
			originalOnToolUse(tu)
		}
		callback = &wrapped
	}

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payload.ProfileArn = profileArn
		} else {
			accountEmail := "<nil>"
			if account != nil {
				accountEmail = account.Email
			}
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmail, err)
		}
	}

	// Build endpoint list ordered by configuration.
	endpoints := getSortedEndpoints(config.GetPreferredEndpoint())

	var lastErr error
	for _, ep := range endpoints {
		// Update the origin field for the selected endpoint.
		payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

		reqBody, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", ep.URL, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = err
			continue
		}

		host := ""
		if parsedURL, parseErr := url.Parse(ep.URL); parseErr == nil {
			host = parsedURL.Host
		}
		headerValues := buildStreamingHeaderValues(account, host)

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "*/*")
		if ep.AmzTarget != "" {
			req.Header.Set("X-Amz-Target", ep.AmzTarget)
		}
		applyKiroBaseHeaders(req, account, headerValues)
		req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
		req.Header.Set("x-amzn-codewhisperer-optout", "true")
		req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
		req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())

		resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
		if err != nil {
			lastErr = err
			logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
			continue
		}

		if resp.StatusCode == 429 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := strings.TrimSpace(string(errBody))
			lastErr = &KiroAPIError{StatusCode: resp.StatusCode, Endpoint: ep.Name, Body: body}
			entry := recordKiro429ProbeLog(ep.Name, account, body)
			logger.Warnf("[Kiro429Probe] endpoint=%q class=%q accountID=%q email=%q tier=%q bodyBytes=%d retainedFor=%s", entry.Endpoint, entry.Class, entry.AccountID, entry.Email, entry.Tier, len(body), kiro429ProbeTTL)
			logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), trying next endpoint", ep.Name)
			continue
		}

		if resp.StatusCode != 200 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = &KiroAPIError{StatusCode: resp.StatusCode, Endpoint: ep.Name, Body: strings.TrimSpace(string(errBody))}
			// Authentication errors and payment errors are not retried across endpoints.
			if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
				return lastErr
			}
			logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
			continue
		}

		err = parseEventStream(resp.Body, callback)
		resp.Body.Close()
		return err
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all endpoints failed")
}

// ==================== Event Stream Parsing ====================

// parseEventStream decodes an AWS binary Event Stream response body.
func parseEventStream(body io.Reader, callback *KiroStreamCallback) error {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	// Read directly without bufio to avoid buffering latency in streaming responses.
	var inputTokens, outputTokens int
	var totalCredits float64
	var currentToolUse *toolUseState
	var lastAssistantContent string
	var lastReasoningContent string

	for {
		// Prelude: 12 bytes (total_len + headers_len + crc)
		prelude := make([]byte, 12)
		_, err := io.ReadFull(body, prelude)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])

		if totalLength < 16 {
			continue
		}

		// Read the remaining message bytes.
		remaining := totalLength - 12
		msgBuf := make([]byte, remaining)
		_, err = io.ReadFull(body, msgBuf)
		if err != nil {
			return err
		}

		if headersLength > len(msgBuf)-4 {
			continue
		}

		eventType := extractEventType(msgBuf[0:headersLength])
		payloadBytes := msgBuf[headersLength : len(msgBuf)-4]
		if len(payloadBytes) == 0 {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal(payloadBytes, &event); err != nil {
			continue
		}

		inputTokens, outputTokens = updateTokensFromEvent(event, inputTokens, outputTokens)

		// Dispatch by event type.
		switch eventType {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				normalized := normalizeChunk(content, &lastAssistantContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, false)
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				normalized := normalizeChunk(text, &lastReasoningContent)
				if normalized != "" && callback.OnText != nil {
					callback.OnText(normalized, true)
				}
			}
		case "toolUseEvent":
			currentToolUse = handleToolUseEvent(event, currentToolUse, callback)
		case "meteringEvent":
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
			}
		case "contextUsageEvent":
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				if callback.OnContextUsage != nil {
					callback.OnContextUsage(pct)
				}
			}
		}
	}

	if currentToolUse != nil {
		finishToolUse(currentToolUse, callback)
	}

	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}

	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	return nil
}

func updateTokensFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int) {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)

	inputTokens := currentInputTokens
	outputTokens := currentOutputTokens

	for _, usage := range candidates {
		if usage == nil {
			continue
		}

		if v, ok := readTokenNumber(usage,
			"outputTokens", "completionTokens", "totalOutputTokens",
			"output_tokens", "completion_tokens", "total_output_tokens",
		); ok {
			outputTokens = v
		}

		if v, ok := readTokenNumber(usage,
			"inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens",
		); ok {
			inputTokens = v
			continue
		}

		uncached, _ := readTokenNumber(usage, "uncachedInputTokens", "uncached_input_tokens")
		cacheRead, _ := readTokenNumber(usage, "cacheReadInputTokens", "cache_read_input_tokens")
		cacheWrite, _ := readTokenNumber(usage, "cacheWriteInputTokens", "cache_write_input_tokens", "cacheCreationInputTokens", "cache_creation_input_tokens")
		if uncached+cacheRead+cacheWrite > 0 {
			inputTokens = uncached + cacheRead + cacheWrite
			continue
		}

		total, ok := readTokenNumber(usage, "totalTokens", "total_tokens")
		if ok && total > 0 {
			candidateOutput := outputTokens
			if v, vok := readTokenNumber(usage,
				"outputTokens", "completionTokens", "totalOutputTokens",
				"output_tokens", "completion_tokens", "total_output_tokens",
			); vok {
				candidateOutput = v
			}
			if total-candidateOutput > 0 {
				inputTokens = total - candidateOutput
			}
		}
	}

	return inputTokens, outputTokens
}

// getContextWindowSize returns the context window size (in tokens) for a model.
func getContextWindowSize(model string) int {
	m := strings.ToLower(model)
	// sonnet-4.6, opus-4.6, opus-4.7 all have 1M context windows
	if strings.Contains(m, "4.6") || strings.Contains(m, "4-6") ||
		strings.Contains(m, "4.7") || strings.Contains(m, "4-7") {
		return 1_000_000
	}
	return 200_000
}

func collectUsageMaps(v interface{}, out *[]map[string]interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "usage" || lk == "tokenusage" || lk == "token_usage" {
				if m, ok := child.(map[string]interface{}); ok {
					*out = append(*out, m)
				}
			}
			collectUsageMaps(child, out)
		}
	case []interface{}:
		for _, child := range t {
			collectUsageMaps(child, out)
		}
	}
}

func normalizeChunk(chunk string, previous *string) string {
	if chunk == "" {
		return ""
	}

	prev := *previous
	if prev == "" {
		*previous = chunk
		return chunk
	}

	if chunk == prev {
		return ""
	}

	if strings.HasPrefix(chunk, prev) {
		delta := chunk[len(prev):]
		*previous = chunk
		return delta
	}

	if strings.HasPrefix(prev, chunk) {
		return ""
	}

	maxOverlap := 0
	maxLen := len(prev)
	if len(chunk) < maxLen {
		maxLen = len(chunk)
	}
	for i := maxLen; i > 0; i-- {
		if strings.HasSuffix(prev, chunk[:i]) {
			maxOverlap = i
			break
		}
	}

	*previous = chunk
	if maxOverlap > 0 {
		return chunk[maxOverlap:]
	}

	return chunk
}

func readTokenNumber(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				return int(parsed), true
			}
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				return parsed, true
			}
			if parsed, err := strconv.ParseFloat(n, 64); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// ==================== Tool Use Handling ====================

type toolUseState struct {
	ToolUseID   string
	Name        string
	InputBuffer strings.Builder
	GeneratedID bool
}

func handleToolUseEvent(event map[string]interface{}, current *toolUseState, callback *KiroStreamCallback) *toolUseState {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")

	if toolUseID != "" && name != "" {
		if current == nil {
			current = &toolUseState{ToolUseID: toolUseID, Name: name}
		} else if current.ToolUseID != toolUseID {
			if current.GeneratedID && current.Name == name {
				current.ToolUseID = toolUseID
				current.GeneratedID = false
			} else {
				finishToolUse(current, callback)
				current = &toolUseState{ToolUseID: toolUseID, Name: name}
			}
		}
	} else if name != "" && current == nil {
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	} else if name != "" && current != nil && current.Name != name {
		finishToolUse(current, callback)
		current = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
	}

	if current != nil {
		if input, ok := event["input"].(string); ok {
			current.InputBuffer.WriteString(input)
		} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
			data, _ := json.Marshal(inputObj)
			current.InputBuffer.Reset()
			current.InputBuffer.Write(data)
		}
	}

	if isStop && current != nil {
		finishToolUse(current, callback)
		return nil
	}

	return current
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) {
	if state == nil || state.Name == "" || callback == nil || callback.OnToolUse == nil {
		return
	}
	if state.ToolUseID == "" {
		state.ToolUseID = "toolu_" + uuid.New().String()
	}
	var input map[string]interface{}
	if state.InputBuffer.Len() > 0 {
		json.Unmarshal([]byte(state.InputBuffer.String()), &input)
	}
	if input == nil {
		input = make(map[string]interface{})
	}
	callback.OnToolUse(KiroToolUse{
		ToolUseID: state.ToolUseID,
		Name:      state.Name,
		Input:     input,
	})
}

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstBoolField(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if v, ok := m[key].(bool); ok {
			return v
		}
	}
	return false
}

// extractEventType extracts the event type string from AWS Event Stream message headers.
func extractEventType(headers []byte) string {
	offset := 0
	for offset < len(headers) {
		if offset >= len(headers) {
			break
		}
		nameLen := int(headers[offset])
		offset++
		if offset+nameLen > len(headers) {
			break
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(headers) {
			break
		}
		valueType := headers[offset]
		offset++

		if valueType == 7 { // String
			if offset+2 > len(headers) {
				break
			}
			valueLen := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2
			if offset+valueLen > len(headers) {
				break
			}
			value := string(headers[offset : offset+valueLen])
			offset += valueLen
			if name == ":event-type" {
				return value
			}
			continue
		}

		// Skip other value types by their fixed byte widths.
		skipSizes := map[byte]int{0: 0, 1: 0, 2: 1, 3: 2, 4: 4, 5: 8, 8: 8, 9: 16}
		if valueType == 6 {
			if offset+2 > len(headers) {
				break
			}
			l := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2 + l
		} else if skip, ok := skipSizes[valueType]; ok {
			offset += skip
		} else {
			break
		}
	}
	return ""
}
