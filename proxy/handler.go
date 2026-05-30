package proxy

import (
	"encoding/json"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const tokenRefreshSkewSeconds int64 = 120

// Handler HTTP 处理器
type Handler struct {
	pool *pool.AccountPool
	// 运行时统计 (使用原子操作)
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	totalTokens     int64
	totalCredits    float64 // float64 需要用锁保护
	creditsMu       sync.RWMutex
	startTime       int64
	stopRefresh     chan struct{}
	stopStatsSaver  chan struct{}
	// 模型缓存
	cachedModels    []ModelInfo
	modelsCacheMu   sync.RWMutex
	modelsCacheTime int64
	promptCache     *promptCacheTracker
	tokenRefreshMu  sync.Mutex
}

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func NewHandler() *Handler {
	// 启动时应用代理配置
	applyProxyConfig(config.GetProxyURL())

	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	h := &Handler{
		pool:            pool.GetPool(),
		totalRequests:   int64(totalReq),
		successRequests: int64(successReq),
		failedRequests:  int64(failedReq),
		totalTokens:     int64(totalTokens),
		totalCredits:    totalCredits,
		startTime:       time.Now().Unix(),
		stopRefresh:     make(chan struct{}),
		stopStatsSaver:  make(chan struct{}),
		promptCache:     newPromptCacheTracker(defaultPromptCacheTTL),
	}
	// 启动后台刷新
	go h.backgroundRefresh()
	// 启动后台统计保存 (每30秒保存一次)
	go h.backgroundStatsSaver()
	startMetricsBackgroundFlush(h.stopStatsSaver)
	// 清理过期的 stored responses（>30 天）
	go purgeExpiredResponses(responsesDefaultTTL)
	return h
}

// backgroundRefresh 后台定时刷新账户信息
func (h *Handler) backgroundRefresh() {
	ticker := time.NewTicker(30 * time.Minute) // 每 30 分钟刷新一次
	defer ticker.Stop()

	// 启动时延迟 10 秒后执行一次
	time.Sleep(10 * time.Second)
	h.refreshModelsCache()
	h.refreshAllAccounts()

	for {
		select {
		case <-ticker.C:
			h.refreshModelsCache()
			h.refreshAllAccounts()
		case <-h.stopRefresh:
			return
		}
	}
}

// refreshAllAccounts 刷新所有账户信息
func (h *Handler) refreshAllAccounts() {
	accounts := config.GetAccounts()
	for i := range accounts {
		account := &accounts[i]
		if !account.Enabled || account.AccessToken == "" {
			continue
		}

		// 检查 token 是否需要刷新
		if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
			newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
			if err != nil {
				logger.Warnf("[BackgroundRefresh] Token refresh failed for %s: %v", account.Email, err)
				h.handleAccountFailure(account, err)
				continue
			}
			account.AccessToken = newAccessToken
			if newRefreshToken != "" {
				account.RefreshToken = newRefreshToken
			}
			account.ExpiresAt = newExpiresAt
			config.UpdateAccountToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
			h.pool.UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
			if profileArn != "" {
				account.ProfileArn = profileArn
				config.UpdateAccountProfileArn(account.ID, profileArn)
			}
		}

		// 刷新账户信息
		info, err := RefreshAccountInfo(account)
		if err != nil {
			logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
			continue
		}

		config.UpdateAccountInfo(account.ID, *info)
		h.refreshAccountOverageIfExceeded(account, info)
		logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
	}
	h.pool.Reload()
}

func (h *Handler) refreshAccountOverageIfExceeded(account *config.Account, info *config.AccountInfo) {
	if account == nil || info == nil || info.UsageLimit <= 0 || info.UsageCurrent <= info.UsageLimit {
		return
	}
	snap, err := FetchOverageStatus(account)
	if err != nil {
		logger.Warnf("[Overage] failed to refresh overage status after usage exceeded for %s: %v", account.Email, err)
		return
	}
	if overagePoints := info.UsageCurrent - info.UsageLimit; overagePoints > snap.CurrentOverages {
		snap.CurrentOverages = overagePoints
	}
	if snap.OverageCap <= 0 {
		snap.OverageCap = 10000
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[Overage] failed to persist overage status after usage exceeded for %s: %v", account.Email, persistErr)
	}
}

// validateApiKey 验证 API Key（Bool 包装，旧签名仍被部分调用方使用）
func (h *Handler) validateApiKey(r *http.Request) bool {
	_, err := h.authenticate(r)
	return err == nil
}

// authenticateForClaude runs authenticate and writes a Claude-style error on failure.
// Returns the request with the matched API key injected into context, or nil if auth failed.
func (h *Handler) authenticateForClaude(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendClaudeError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// authenticateForOpenAI runs authenticate and writes an OpenAI-style error on failure.
func (h *Handler) authenticateForOpenAI(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendOpenAIError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] %s %s from %s", r.Method, path, r.RemoteAddr)

	// CORS - 完整的头部支持
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
	w.Header().Set("Access-Control-Expose-Headers", "x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens")

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// 路由
	switch {
	// API 端点（需要验证 API Key）
	case path == "/v1/messages" || path == "/messages" || path == "/anthropic/v1/messages":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleClaudeMessages(w, ar)
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleCountTokens(w, ar)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIChat(w, ar)
	case path == "/v1/responses" || path == "/responses":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIResponses(w, ar)
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
	case path == "/api/event_logging/batch":
		// Claude Code 遥测端点 - 直接返回 200 OK
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// 管理端点
	case path == "/admin" || path == "/admin/":
		h.serveAdminPage(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		h.handleAdminAPI(w, r)
	case strings.HasPrefix(path, "/admin/"):
		h.serveStaticFile(w, r)

	// 健康检查
	case path == "/health" || path == "/":
		h.handleHealth(w, r)

	// 统计端点（需要 API Key 鉴权）
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// handleHealth 健康检查（不暴露统计数据）
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": config.Version,
		"uptime":  time.Now().Unix() - h.startTime,
	})
}

// handleStats 统计数据（需要 API Key 鉴权）
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// saveStats 保存统计到配置文件
func (h *Handler) saveStats() {
	config.UpdateStats(
		int(atomic.LoadInt64(&h.totalRequests)),
		int(atomic.LoadInt64(&h.successRequests)),
		int(atomic.LoadInt64(&h.failedRequests)),
		int(atomic.LoadInt64(&h.totalTokens)),
		h.getCredits(),
	)
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// 统计记录 (使用原子操作)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
}

// recordSuccessForApiKey is recordSuccess + per-API-key usage attribution.
// When apiKeyID is empty (legacy single-key path or unauthenticated path), only the
// global counters are updated. Persistence errors are logged but do not propagate.
func (h *Handler) recordSuccessForApiKey(apiKeyID string, inputTokens, outputTokens int, credits float64) {
	h.recordSuccess(inputTokens, outputTokens, credits)
	if apiKeyID == "" {
		return
	}
	if err := config.RecordApiKeyUsage(apiKeyID, int64(inputTokens+outputTokens), credits); err != nil {
		logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
	}
}

func (h *Handler) recordFailure() {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)
}

// ==================== 管理 API ====================

// ==================== 静态文件服务 ====================

func setAdminNoCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	setAdminNoCacheHeaders(w)
	http.ServeFile(w, r, "web/index.html")
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	setAdminNoCacheHeaders(w)
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	http.ServeFile(w, r, "web/"+path)
}

// applyProxyConfig 将代理配置应用到所有出站 HTTP 客户端（Kiro API + auth 模块）
func applyProxyConfig(proxyURL string) {
	InitKiroHttpClient(proxyURL)
	auth.InitHttpClient(proxyURL)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
