// Package pool 账号池管理
// 实现轮询负载均衡、错误冷却、Token 刷新
package pool

import (
	"context"
	"errors"
	"kiro-go/config"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const tokenRefreshSkewSeconds int64 = 120
const routingEventWindow = time.Hour

var (
	ErrRoutingQueueFull    = errors.New("routing queue full")
	ErrRoutingQueueTimeout = errors.New("routing queue timeout")
	ErrRoutingUnavailable  = errors.New("no available accounts")
)

type requestEvent struct {
	at      time.Time
	is429   bool
	isError bool
}

type AccountHealthSnapshot struct {
	ID           string
	Requests     int     `json:"requests"`
	QuotaErrors  int     `json:"quotaErrors"`
	ErrorCount   int     `json:"errorCount"`
	Rate429      float64 `json:"rate429"`
	StablePool   bool    `json:"stablePool"`
	HealthScore  int     `json:"healthScore"`
	LastErrorAt  int64   `json:"lastErrorAt,omitempty"`
	CoolingUntil int64   `json:"coolingUntil,omitempty"`
	CanRoute     bool    `json:"canRoute"`
	ModeBucket   string  `json:"modeBucket,omitempty"`
}

// AccountPool 账号池
type AccountPool struct {
	mu                      sync.RWMutex
	accounts                []config.Account
	totalAccounts           int
	currentIndex            uint64
	cooldowns               map[string]time.Time       // 账号冷却时间
	errorCounts             map[string]int             // 连续错误计数
	modelLists              map[string]map[string]bool // accountID → set of modelIDs (from ListAvailableModels)
	requestLog              map[string][]requestEvent  // accountID → rolling request events (last hour)
	lastErrorAt             map[string]time.Time       // accountID → last error timestamp
	routeActiveByAccount    map[string]int
	routeLastStartByAccount map[string]time.Time
	routeStickyByKey        map[string]string
	routeGlobalActive       int
	routeWaiting            int
	routeNotify             chan struct{}
	lastAutoRestoreRefresh  time.Time
	autoRestoreRefresh      bool

	// Cumulative routing counters (lifetime since process start).
	routeEnqueuedTotal  uint64 // requests that had to wait in the queue at least once
	routeProcessedTotal uint64 // requests that successfully acquired a route slot
	routeRejectedTotal  uint64 // requests rejected because the queue was full
	routeTimeoutTotal   uint64 // requests that timed out while waiting in the queue
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:               make(map[string]time.Time),
			errorCounts:             make(map[string]int),
			modelLists:              make(map[string]map[string]bool),
			requestLog:              make(map[string][]requestEvent),
			lastErrorAt:             make(map[string]time.Time),
			routeActiveByAccount:    make(map[string]int),
			routeLastStartByAccount: make(map[string]time.Time),
			routeStickyByKey:        make(map[string]string),
			routeNotify:             make(chan struct{}),
			autoRestoreRefresh:      true,
		}
		pool.Reload()
	})
	return pool
}

// Reload 从配置重新加载账号
// 构建加权列表：weight<=1 出现 1 次，weight>=2 出现 weight 次。
// 额度耗尽的账号是否参与调度由上游 OverageStatus 决定（DISABLED → 跳过）。
func (p *AccountPool) Reload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reloadLocked()
}

func (p *AccountPool) reloadLocked() {
	enabled := config.GetEnabledAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	var weighted []config.Account
	for _, a := range enabled {
		if isQuotaBlocked(a, allowOverUsage) {
			continue
		}
		w := effectiveWeight(a.Weight)
		for j := 0; j < w; j++ {
			weighted = append(weighted, a)
		}
	}
	p.accounts = weighted
	p.totalAccounts = len(enabled)
	p.ensureRuntimeMapsLocked()
	p.notifyRouteWaitersLocked()
}

func (p *AccountPool) refreshAutoRestoredAccounts() {
	if !p.autoRestoreRefresh {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.lastAutoRestoreRefresh.IsZero() && time.Since(p.lastAutoRestoreRefresh) < time.Minute {
		return
	}
	p.lastAutoRestoreRefresh = time.Now()
	p.reloadLocked()
}

// GetNext 获取下一个可用账号
func (p *AccountPool) GetNext() *config.Account {
	return p.GetNextExcluding(nil)
}

// GetNextExcluding 获取下一个可用账号（加权轮询），并跳过指定账号。
func (p *AccountPool) GetNextExcluding(excluded map[string]bool) *config.Account {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.getNextLockedExcept("", excluded)
}

// GetNextExcept 获取下一个可用账号，并排除已尝试的账号。
func (p *AccountPool) GetNextExcept(exclude map[string]bool) *config.Account {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.getNextLockedExcept("", exclude)
}

// SetModelList 缓存账号支持的模型集合（由 handler 在刷新后调用）
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		set[strings.ToLower(strings.TrimSpace(id))] = true
	}
	p.mu.Lock()
	p.modelLists[accountID] = set
	p.mu.Unlock()
}

// GetModelList 返回该账号缓存的模型 ID 列表（供 admin API 使用）。
// 若尚无缓存则返回空切片。
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// accountHasModel 检查账号是否支持指定模型。
// 若该账号尚无模型列表（冷启动），视为支持所有模型。
func (p *AccountPool) accountHasModel(accountID, model string) bool {
	list, ok := p.modelLists[accountID]
	if !ok || len(list) == 0 {
		return true // 冷启动：列表未就绪，乐观放行
	}
	return list[strings.ToLower(strings.TrimSpace(model))]
}

// GetNextForModel 获取下一个支持指定模型的可用账号。
// model 应为去掉 thinking 后缀的实际模型名。
// 若无账号有该模型列表数据，行为与 GetNext 相同（乐观路由）。
func (p *AccountPool) GetNextForModel(model string) *config.Account {
	return p.GetNextForModelExcluding(model, nil)
}

// GetNextForModelExcluding 获取下一个支持指定模型的可用账号，并跳过指定账号。
func (p *AccountPool) GetNextForModelExcluding(model string, excluded map[string]bool) *config.Account {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.getNextLockedExcept(model, excluded)
}

// GetNextForModelExcept 获取下一个支持指定模型的可用账号，并排除已尝试账号。
func (p *AccountPool) GetNextForModelExcept(model string, exclude map[string]bool) *config.Account {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.getNextLockedExcept(model, exclude)
}

func needsTokenRefresh(acc config.Account, now time.Time) bool {
	return acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds
}

func hasRefreshToken(acc config.Account) bool {
	return strings.TrimSpace(acc.RefreshToken) != ""
}

func canRouteByToken(acc config.Account, now time.Time) bool {
	return !needsTokenRefresh(acc, now) || hasRefreshToken(acc)
}

func (p *AccountPool) getNextLockedExcept(model string, exclude map[string]bool) *config.Account {
	if len(p.accounts) == 0 {
		return nil
	}

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	n := len(p.accounts)
	seen := make(map[string]bool)

	// 加权轮询查找可用账号
	for i := 0; i < n; i++ {
		idx := atomic.AddUint64(&p.currentIndex, 1) % uint64(n)
		acc := &p.accounts[idx]

		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if !p.canRouteAccountLocked(acc, model, exclude, allowOverUsage, now, true) {
			continue
		}
		return acc
	}

	// fallback：找冷却时间最短且仍满足 token/model/额度约束的账号
	var best *config.Account
	var earliest time.Time
	seenFallback := make(map[string]bool)
	for i := range p.accounts {
		acc := &p.accounts[i]
		if seenFallback[acc.ID] {
			continue
		}
		seenFallback[acc.ID] = true
		if exclude != nil && exclude[acc.ID] {
			continue
		}
		if model != "" && !p.accountHasModel(acc.ID, model) {
			continue
		}
		if !canRouteByToken(*acc, now) {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return acc
		}
	}
	return best
}

func (p *AccountPool) canRouteAccountLocked(acc *config.Account, model string, exclude map[string]bool, allowOverUsage bool, now time.Time, skipCooling bool) bool {
	if acc == nil {
		return false
	}
	if exclude != nil && exclude[acc.ID] {
		return false
	}
	if model != "" && !p.accountHasModel(acc.ID, model) {
		return false
	}
	if skipCooling {
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			return false
		}
	}
	if !canRouteByToken(*acc, now) {
		return false
	}
	if isQuotaBlocked(*acc, allowOverUsage) {
		return false
	}
	return true
}

type routingTryResult struct {
	account *config.Account
	wait    time.Duration
	notify  <-chan struct{}
	busy    bool
}

func (p *AccountPool) AcquireForModel(ctx context.Context, model string, excluded map[string]bool, affinityKey string) (*config.Account, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rc := config.GetRoutingConcurrencyConfig()
	if !rc.Enabled {
		acc := p.GetNextForModelExcluding(model, excluded)
		if acc == nil {
			return nil, nil, ErrRoutingUnavailable
		}
		atomic.AddUint64(&p.routeProcessedTotal, 1)
		return acc, func() {}, nil
	}

	queueTimeout := time.Duration(rc.GlobalQueueTimeoutMs) * time.Millisecond
	if queueTimeout <= 0 {
		queueTimeout = 30 * time.Second
	}
	deadline := time.NewTimer(queueTimeout)
	defer deadline.Stop()
	queued := false
	defer func() {
		if queued {
			p.mu.Lock()
			if p.routeWaiting > 0 {
				p.routeWaiting--
			}
			p.mu.Unlock()
		}
	}()

	for {
		res, err := p.tryAcquireForModel(model, excluded, affinityKey, rc)
		if err != nil {
			return nil, nil, err
		}
		if res.account != nil {
			atomic.AddUint64(&p.routeProcessedTotal, 1)
			return res.account, p.releaseRouteFunc(res.account.ID), nil
		}
		if !res.busy {
			return nil, nil, ErrRoutingUnavailable
		}
		if !queued {
			if rc.GlobalQueueSize <= 0 {
				atomic.AddUint64(&p.routeRejectedTotal, 1)
				return nil, nil, ErrRoutingQueueFull
			}
			p.mu.Lock()
			if p.routeWaiting >= rc.GlobalQueueSize {
				p.mu.Unlock()
				atomic.AddUint64(&p.routeRejectedTotal, 1)
				return nil, nil, ErrRoutingQueueFull
			}
			p.routeWaiting++
			queued = true
			p.mu.Unlock()
			atomic.AddUint64(&p.routeEnqueuedTotal, 1)
		}

		var intervalC <-chan time.Time
		var timer *time.Timer
		if res.wait > 0 {
			timer = time.NewTimer(res.wait)
			intervalC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil, nil, ctx.Err()
		case <-deadline.C:
			if timer != nil {
				timer.Stop()
			}
			atomic.AddUint64(&p.routeTimeoutTotal, 1)
			return nil, nil, ErrRoutingQueueTimeout
		case <-res.notify:
			if timer != nil {
				timer.Stop()
			}
		case <-intervalC:
		}
	}
}

func (p *AccountPool) tryAcquireForModel(model string, excluded map[string]bool, affinityKey string, rc config.RoutingConcurrencyConfig) (routingTryResult, error) {
	p.refreshAutoRestoredAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureRuntimeMapsLocked()
	if len(p.accounts) == 0 {
		return routingTryResult{}, ErrRoutingUnavailable
	}
	if rc.GlobalMaxConcurrent > 0 && p.routeGlobalActive >= rc.GlobalMaxConcurrent {
		return routingTryResult{busy: true, notify: p.routeNotify}, nil
	}

	var earliestWait time.Duration
	busySeen := false
	tryAccount := func(acc *config.Account) (*config.Account, bool) {
		if !p.canRouteAccountLocked(acc, model, excluded, allowOverUsage, now, true) {
			return nil, false
		}
		if p.routeActiveByAccount[acc.ID] >= rc.PerAccountMaxConcurrent {
			busySeen = true
			return nil, true
		}
		if rc.PerAccountMinIntervalMs > 0 {
			minInterval := time.Duration(rc.PerAccountMinIntervalMs) * time.Millisecond
			if lastStart, ok := p.routeLastStartByAccount[acc.ID]; ok {
				if wait := minInterval - now.Sub(lastStart); wait > 0 {
					busySeen = true
					if earliestWait <= 0 || wait < earliestWait {
						earliestWait = wait
					}
					return nil, true
				}
			}
		}
		p.routeGlobalActive++
		p.routeActiveByAccount[acc.ID]++
		p.routeLastStartByAccount[acc.ID] = now
		if rc.StickyAccount && strings.TrimSpace(affinityKey) != "" {
			p.routeStickyByKey[affinityKey] = acc.ID
		}
		return acc, true
	}

	stickyID := ""
	if rc.StickyAccount && strings.TrimSpace(affinityKey) != "" {
		stickyID = p.routeStickyByKey[affinityKey]
	}
	if stickyID != "" {
		for i := range p.accounts {
			if p.accounts[i].ID != stickyID {
				continue
			}
			if acc, considered := tryAccount(&p.accounts[i]); acc != nil || considered {
				if acc != nil || !rc.OverflowToOtherAccounts {
					return routingTryResult{account: acc, busy: acc == nil, wait: earliestWait, notify: p.routeNotify}, nil
				}
				break
			}
			if !rc.OverflowToOtherAccounts {
				return routingTryResult{}, ErrRoutingUnavailable
			}
			break
		}
	}

	n := len(p.accounts)
	seen := make(map[string]bool)
	for i := 0; i < n; i++ {
		idx := atomic.AddUint64(&p.currentIndex, 1) % uint64(n)
		acc := &p.accounts[idx]
		if seen[acc.ID] || (stickyID != "" && acc.ID == stickyID) {
			continue
		}
		seen[acc.ID] = true
		if selected, _ := tryAccount(acc); selected != nil {
			return routingTryResult{account: selected}, nil
		}
	}
	return routingTryResult{busy: busySeen, wait: earliestWait, notify: p.routeNotify}, nil
}

func (p *AccountPool) releaseRouteFunc(accountID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.ensureRuntimeMapsLocked()
			if p.routeActiveByAccount[accountID] > 0 {
				p.routeActiveByAccount[accountID]--
				if p.routeActiveByAccount[accountID] == 0 {
					delete(p.routeActiveByAccount, accountID)
				}
			}
			if p.routeGlobalActive > 0 {
				p.routeGlobalActive--
			}
			p.notifyRouteWaitersLocked()
		})
	}
}

func (p *AccountPool) notifyRouteWaitersLocked() {
	old := p.routeNotify
	p.routeNotify = make(chan struct{})
	close(old)
}

func (p *AccountPool) RoutingStats() map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()
	perAccount := make(map[string]int, len(p.routeActiveByAccount))
	for id, n := range p.routeActiveByAccount {
		if n > 0 {
			perAccount[id] = n
		}
	}
	return map[string]interface{}{
		"active":           p.routeGlobalActive,
		"waiting":          p.routeWaiting,
		"perAccountActive": perAccount,
		"enqueuedTotal":    atomic.LoadUint64(&p.routeEnqueuedTotal),
		"processedTotal":   atomic.LoadUint64(&p.routeProcessedTotal),
		"rejectedTotal":    atomic.LoadUint64(&p.routeRejectedTotal),
		"timeoutTotal":     atomic.LoadUint64(&p.routeTimeoutTotal),
	}
}

func (p *AccountPool) getRecentStatsLocked(id string, now time.Time) (requests int, quotaErrors int, rate429 float64) {
	events := p.requestLog[id]
	if len(events) == 0 {
		return 0, 0, 0
	}
	cutoff := now.Add(-routingEventWindow)
	for _, event := range events {
		if event.at.Before(cutoff) {
			continue
		}
		requests++
		if event.is429 {
			quotaErrors++
		}
	}
	if requests > 0 {
		rate429 = float64(quotaErrors) / float64(requests)
	}
	return
}

func (p *AccountPool) pruneRequestLogLocked(id string, now time.Time) {
	events := p.requestLog[id]
	if len(events) == 0 {
		return
	}
	cutoff := now.Add(-routingEventWindow)
	idx := 0
	for idx < len(events) && events[idx].at.Before(cutoff) {
		idx++
	}
	if idx == 0 {
		return
	}
	if idx >= len(events) {
		delete(p.requestLog, id)
		return
	}
	p.requestLog[id] = append([]requestEvent(nil), events[idx:]...)
}

func (p *AccountPool) ensureRuntimeMapsLocked() {
	if p.cooldowns == nil {
		p.cooldowns = make(map[string]time.Time)
	}
	if p.errorCounts == nil {
		p.errorCounts = make(map[string]int)
	}
	if p.modelLists == nil {
		p.modelLists = make(map[string]map[string]bool)
	}
	if p.requestLog == nil {
		p.requestLog = make(map[string][]requestEvent)
	}
	if p.lastErrorAt == nil {
		p.lastErrorAt = make(map[string]time.Time)
	}
	if p.routeActiveByAccount == nil {
		p.routeActiveByAccount = make(map[string]int)
	}
	if p.routeLastStartByAccount == nil {
		p.routeLastStartByAccount = make(map[string]time.Time)
	}
	if p.routeStickyByKey == nil {
		p.routeStickyByKey = make(map[string]string)
	}
	if p.routeNotify == nil {
		p.routeNotify = make(chan struct{})
	}
}

func (p *AccountPool) appendRequestEventLocked(id string, is429 bool, isError bool) {
	p.ensureRuntimeMapsLocked()
	now := time.Now()
	p.pruneRequestLogLocked(id, now)
	p.requestLog[id] = append(p.requestLog[id], requestEvent{at: now, is429: is429, isError: isError})
	if isError {
		p.lastErrorAt[id] = now
	}
}

func (p *AccountPool) computeHealthScoreLocked(acc *config.Account, requests int, quotaErrors int, rate429 float64, now time.Time) int {
	score := 55
	score += subscriptionRank(*acc) * 6
	score += minInt(20, int((1.0-acc.UsagePercent)*20))
	score += minInt(8, effectiveWeight(acc.Weight)-1)
	score -= p.errorCounts[acc.ID] * 12
	score -= int(rate429 * 45)
	if isOverUsageLimit(*acc) {
		score -= 12
	}
	if lastErr, ok := p.lastErrorAt[acc.ID]; ok {
		if now.Sub(lastErr) < 10*time.Minute {
			score -= 10
		}
	}
	if requests == 0 {
		score += 4
	}
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

func subscriptionRank(acc config.Account) int {
	tier := strings.ToUpper(strings.TrimSpace(acc.SubscriptionType + " " + acc.SubscriptionTitle))
	switch {
	case strings.Contains(tier, "PRO_PLUS"), strings.Contains(tier, "PROPLUS"), strings.Contains(tier, "PRO+"):
		return 4
	case strings.Contains(tier, "POWER"):
		return 3
	case strings.Contains(tier, "PRO"):
		return 2
	default:
		return 1
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return &p.accounts[i]
		}
	}
	return nil
}

// RecordSuccess 记录请求成功，清除冷却
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureRuntimeMapsLocked()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
	p.appendRequestEventLocked(id, false, false)
}

// RestoreAccount marks a previously disabled/cooling account usable again after a
// successful manual test.
func (p *AccountPool) RestoreAccount(id string) {
	_ = config.SetAccountEnabled(id, true)
	p.mu.Lock()
	p.ensureRuntimeMapsLocked()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
	delete(p.lastErrorAt, id)
	p.requestLog[id] = nil
	p.mu.Unlock()
	p.Reload()
}

// RecordTransient429 records a retryable upstream 429 without disabling or cooling
// the account. These events are used for recent-429 visibility and health scoring,
// but the account remains eligible for subsequent requests.
func (p *AccountPool) RecordTransient429(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureRuntimeMapsLocked()
	p.appendRequestEventLocked(id, true, true)
}

// RecordError 记录请求错误，设置冷却
func (p *AccountPool) RecordError(id string, isQuotaError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureRuntimeMapsLocked()

	p.errorCounts[id]++
	p.appendRequestEventLocked(id, isQuotaError, true)

	if isQuotaError {
		// 配额错误，冷却 1 小时
		p.cooldowns[id] = time.Now().Add(time.Hour)
	} else if p.errorCounts[id] >= 3 {
		// 连续 3 次错误，冷却 1 分钟
		p.cooldowns[id] = time.Now().Add(time.Minute)
	}
}

// IsAuthFailure reports whether an error indicates the refresh token / credentials
// have been revoked or invalidated upstream (401, 403 with auth markers, etc.).
// These accounts cannot be recovered automatically and must be re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	// Match HTTP status codes only when they appear as standalone tokens to avoid
	// false positives from arbitrary digits in the error body (e.g. request IDs).
	if hasStatusToken(msg, "401") || hasStatusToken(msg, "403") {
		return true
	}
	if strings.Contains(lower, "bad credentials") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid grant") ||
		strings.Contains(lower, "invalid_token") ||
		strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "token expired") ||
		strings.Contains(lower, "token has expired") ||
		strings.Contains(lower, "unauthorized") {
		return true
	}
	return false
}

// hasStatusToken returns true when status appears in s with non-digit boundaries
// on both sides, so "401" matches "HTTP 401 from ..." but not "request_401abc".
func hasStatusToken(s, status string) bool {
	for {
		idx := strings.Index(s, status)
		if idx < 0 {
			return false
		}
		leftOK := idx == 0 || !isDigit(s[idx-1])
		rightIdx := idx + len(status)
		rightOK := rightIdx >= len(s) || !isDigit(s[rightIdx])
		if leftOK && rightOK {
			return true
		}
		s = s[idx+len(status):]
	}
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// IsSuspensionError reports whether the error indicates the account has been
// temporarily suspended by upstream or has no available Kiro profile.
// Unlike auth failures (revoked credentials), these may be transient, but
// the account should be disabled until an operator re-enables it.
func IsSuspensionError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "temporarily_suspended") ||
		strings.Contains(lower, "temporarily suspended") ||
		strings.Contains(lower, "no available kiro profile")
}

// DisableAccount marks an account as disabled (auth revoked / unrecoverable),
// removes it from the in-memory pool so subsequent requests skip it, and
// persists the change via config.SetAccountBanStatus.
func (p *AccountPool) DisableAccount(id, reason string) {
	if err := config.SetAccountBanStatus(id, "DISABLED", reason); err != nil {
		// best effort — even if persistence fails, drop it from memory
		_ = err
	}
	p.mu.Lock()
	// Long cooldown as a safety net in case Reload races
	p.cooldowns[id] = time.Now().Add(24 * time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// QuarantineAccount429 disables an account for the standard 1h suspicious-429
// window while keeping it eligible for automatic restoration afterwards.
func (p *AccountPool) QuarantineAccount429(id string) {
	_ = config.SuspendAccountTemporarily(id, config.AutoQuarantineSuspicious429Reason())
	p.mu.Lock()
	p.ensureRuntimeMapsLocked()
	p.errorCounts[id]++
	p.appendRequestEventLocked(id, true, true)
	p.cooldowns[id] = time.Now().Add(time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// MarkOverLimit marks an account as over usage limit (after a 402 / OVERAGE response).
// With the upstream OverageStatus model, the live status is refreshed via
// FetchOverageStatus from the request handler; here we just cooldown briefly so
// the next attempt picks a different account, then reload.
func (p *AccountPool) MarkOverLimit(id string) {
	p.mu.Lock()
	p.cooldowns[id] = time.Now().Add(time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.totalAccounts > 0 {
		return p.totalAccounts
	}

	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		seen[acc.ID] = true
	}
	return len(seen)
}

// AvailableCount 返回可用账号数
func (p *AccountPool) AvailableCount() int {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	allowOverUsage := config.GetAllowOverUsage()
	count := 0
	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		if !canRouteByToken(acc, now) {
			continue
		}
		if isQuotaBlocked(acc, allowOverUsage) {
			continue
		}
		count++
	}
	return count
}

// UpdateStats 更新账号统计
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var updated bool
	var requestCount, errorCount, totalTokens int
	var totalCredits float64
	var lastUsed int64
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			if !updated {
				p.accounts[i].RequestCount++
				p.accounts[i].TotalTokens += tokens
				p.accounts[i].TotalCredits += credits
				p.accounts[i].LastUsed = time.Now().Unix()

				requestCount = p.accounts[i].RequestCount
				errorCount = p.accounts[i].ErrorCount
				totalTokens = p.accounts[i].TotalTokens
				totalCredits = p.accounts[i].TotalCredits
				lastUsed = p.accounts[i].LastUsed
				updated = true
				continue
			}
			p.accounts[i].RequestCount = requestCount
			p.accounts[i].ErrorCount = errorCount
			p.accounts[i].TotalTokens = totalTokens
			p.accounts[i].TotalCredits = totalCredits
			p.accounts[i].LastUsed = lastUsed
		}
	}
	if updated {
		go config.UpdateAccountStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
	}
}

// GetAllAccounts 获取所有账号副本
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

func (p *AccountPool) GetHealthSnapshots() map[string]AccountHealthSnapshot {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	allowOverUsage := config.GetAllowOverUsage()
	seen := make(map[string]bool)
	result := make(map[string]AccountHealthSnapshot)
	for i := range p.accounts {
		acc := &p.accounts[i]
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		requests, quotaErrors, rate429 := p.getRecentStatsLocked(acc.ID, now)
		canRoute := true
		if !canRouteByToken(*acc, now) {
			canRoute = false
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			canRoute = false
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			canRoute = false
		}
		snapshot := AccountHealthSnapshot{
			ID:          acc.ID,
			Requests:    requests,
			QuotaErrors: quotaErrors,
			ErrorCount:  p.errorCounts[acc.ID],
			Rate429:     rate429,
			StablePool:  requests < 3 || rate429 < 0.2,
			HealthScore: p.computeHealthScoreLocked(acc, requests, quotaErrors, rate429, now),
			CanRoute:    canRoute,
		}
		if lastErr, ok := p.lastErrorAt[acc.ID]; ok {
			snapshot.LastErrorAt = lastErr.Unix()
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			snapshot.CoolingUntil = cooldown.Unix()
		}
		if snapshot.StablePool {
			snapshot.ModeBucket = "stable"
		} else {
			snapshot.ModeBucket = "probe"
		}
		result[acc.ID] = snapshot
	}
	return result
}

func isOverUsageLimit(acc config.Account) bool {
	return acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit
}

// isQuotaBlocked reports whether an over-quota account should be skipped.
// Upstream OverageStatus=ENABLED and global allowOverUsage both keep it routable.
func isQuotaBlocked(acc config.Account, allowOverUsage bool) bool {
	return isOverUsageLimit(acc) && !isUpstreamOverageEnabled(acc) && !allowOverUsage
}

// isUpstreamOverageEnabled reports whether the upstream Overages switch is ON for this account.
// "ENABLED" → true; anything else (DISABLED, UNKNOWN, empty) → false.
func isUpstreamOverageEnabled(acc config.Account) bool {
	return strings.EqualFold(acc.OverageStatus, "ENABLED")
}

func effectiveWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	return weight
}
