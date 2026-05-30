package pool

import (
	"context"
	"errors"
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

func TestOverLimitAccountsAreSkippedByDefault(t *testing.T) {
	p := &AccountPool{}
	normal := config.Account{ID: "normal"}
	overLimit := config.Account{ID: "over", UsageCurrent: 10, UsageLimit: 10}

	p.accounts = []config.Account{normal, overLimit}

	for i := 0; i < 5; i++ {
		acc := p.GetNext()
		if acc == nil {
			t.Fatalf("expected an account")
		}
		if acc.ID == "over" {
			t.Fatalf("expected over-limit account to be skipped when upstream OverageStatus is empty")
		}
	}
}

func TestOverLimitAccountsCanBeSelectedWhenUpstreamOverageEnabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "ENABLED",
	}

	p.accounts = []config.Account{overLimit}

	acc := p.GetNext()
	if acc == nil {
		t.Fatalf("expected upstream-enabled overage account to be selectable")
	}
	if acc.ID != "over" {
		t.Fatalf("expected overage account, got %q", acc.ID)
	}
}

func TestOverLimitAccountsRemainSkippedWhenUpstreamOverageDisabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "DISABLED",
	}

	p.accounts = []config.Account{overLimit}

	if acc := p.GetNext(); acc != nil {
		t.Fatalf("expected nil when upstream OverageStatus=DISABLED, got %q", acc.ID)
	}
}

func TestGetNextKeepsFiveMinuteTokenAvailable(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:          "acct-1",
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 300,
	}

	p.accounts = []config.Account{account}

	got := p.GetNext()
	if got == nil {
		t.Fatalf("expected five-minute token to be available")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

// ---------------------------------------------------------------------------
// IsAuthFailure
// ---------------------------------------------------------------------------

func TestIsAuthFailureRecognizes401And403(t *testing.T) {
	positives := []string{
		"HTTP 401 from server",
		"received 403 Forbidden",
		"bad credentials",
		"invalid_grant",
		"invalid_token",
		"token expired",
		"token has expired",
		"unauthorized",
	}
	for _, msg := range positives {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = false, want true", msg)
		}
	}
}

func TestIsAuthFailureIgnoresFalsePositives(t *testing.T) {
	// hasStatusToken only excludes digit boundaries; e.g. "4011" contains "401"
	// but the trailing '1' is a digit so it does NOT match.
	negatives := []string{
		"status code 4011 found", // digit immediately after 401 → not a standalone token
		"error 14013 exceeded",   // digit before and after 401
		"some random error",
		"status 200 OK",
	}
	for _, msg := range negatives {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = true, want false", msg)
		}
	}
}

func TestIsAuthFailureNilError(t *testing.T) {
	if IsAuthFailure(nil) {
		t.Fatal("IsAuthFailure(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// IsSuspensionError
// ---------------------------------------------------------------------------

func TestIsSuspensionErrorDetectsKnownMessages(t *testing.T) {
	positives := []string{
		"account temporarily_suspended",
		"account temporarily suspended",
		"no available kiro profile",
		"No Available Kiro Profile", // case-insensitive
	}
	for _, msg := range positives {
		if !IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = false, want true", msg)
		}
	}
}

func TestIsSuspensionErrorIgnoresUnrelatedErrors(t *testing.T) {
	negatives := []string{
		"some other error",
		"unauthorized",
		"429 too many requests",
	}
	for _, msg := range negatives {
		if IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = true, want false", msg)
		}
	}
}

func TestIsSuspensionErrorNilError(t *testing.T) {
	if IsSuspensionError(nil) {
		t.Fatal("IsSuspensionError(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// GetNextForModelExcluding
// ---------------------------------------------------------------------------

func newTestPool(accounts ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.accounts = accounts
	return p
}

func TestGetNextForModelExcludingSkipsExcludedAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	excluded := map[string]bool{"a": true}
	for i := 0; i < 5; i++ {
		acc := p.GetNextForModelExcluding("model", excluded)
		if acc == nil {
			t.Fatal("expected account b, got nil")
		}
		if acc.ID == "a" {
			t.Fatalf("excluded account a was returned on iteration %d", i)
		}
	}
}

func TestGetNextForModelExcludingReturnsNilWhenAllExcluded(t *testing.T) {
	p := newTestPool(config.Account{ID: "only"})
	acc := p.GetNextForModelExcluding("model", map[string]bool{"only": true})
	if acc != nil {
		t.Fatalf("expected nil when only account is excluded, got %q", acc.ID)
	}
}

func TestGetNextForModelExcludingReturnsNilOnEmptyPool(t *testing.T) {
	p := newTestPool()
	acc := p.GetNextForModelExcluding("model", map[string]bool{})
	if acc != nil {
		t.Fatalf("expected nil for empty pool, got %q", acc.ID)
	}
}

func TestAcquireForModelHonorsPerAccountConcurrencyAndOverflow(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateRoutingConcurrencyConfig(config.RoutingConcurrencyConfig{
		Enabled:                 true,
		GlobalQueueSize:         0,
		GlobalQueueTimeoutMs:    50,
		PerAccountMaxConcurrent: 1,
		StickyAccount:           true,
		OverflowToOtherAccounts: true,
	}); err != nil {
		t.Fatalf("UpdateRoutingConcurrencyConfig: %v", err)
	}
	p := newTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})

	first, releaseFirst, err := p.AcquireForModel(context.Background(), "", nil, "key")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, releaseSecond, err := p.AcquireForModel(context.Background(), "", nil, "key")
	if err != nil {
		releaseFirst()
		t.Fatalf("second acquire: %v", err)
	}
	defer releaseFirst()
	defer releaseSecond()
	if first.ID == second.ID {
		t.Fatalf("expected overflow to another account, got %q twice", first.ID)
	}
}

func TestAcquireForModelQueueTimeout(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateRoutingConcurrencyConfig(config.RoutingConcurrencyConfig{
		Enabled:                 true,
		GlobalMaxConcurrent:     1,
		GlobalQueueSize:         1,
		GlobalQueueTimeoutMs:    10,
		PerAccountMaxConcurrent: 1,
		StickyAccount:           true,
		OverflowToOtherAccounts: true,
	}); err != nil {
		t.Fatalf("UpdateRoutingConcurrencyConfig: %v", err)
	}
	p := newTestPool(config.Account{ID: "a"})
	_, release, err := p.AcquireForModel(context.Background(), "", nil, "key")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()
	_, _, err = p.AcquireForModel(context.Background(), "", nil, "other-key")
	if !errors.Is(err, ErrRoutingQueueTimeout) {
		t.Fatalf("expected queue timeout, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// DisableAccount
// ---------------------------------------------------------------------------

func TestDisableAccountSetsCooldown(t *testing.T) {
	// Initialize a temporary config so SetAccountBanStatus can persist safely.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := newTestPool()
	p.DisableAccount("test-id", "test reason")

	p.mu.RLock()
	cooldown, ok := p.cooldowns["test-id"]
	p.mu.RUnlock()

	if !ok {
		t.Fatal("expected cooldown to be set after DisableAccount")
	}
	// Safety-net cooldown must be at least 23 hours from now.
	minExpected := time.Now().Add(23 * time.Hour)
	if cooldown.Before(minExpected) {
		t.Fatalf("expected cooldown >= 23h in future, got %v", cooldown)
	}
}

func TestGetNextExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}

	acc := p.GetNextExcluding(map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

func TestGetNextForModelExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}
	p.SetModelList("a", []string{"claude-sonnet-4.5"})
	p.SetModelList("b", []string{"claude-sonnet-4.5"})

	acc := p.GetNextForModelExcluding("claude-sonnet-4.5", map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

// ---------------------------------------------------------------------------
// Reload over-usage filtering
// ---------------------------------------------------------------------------

func TestReloadKeepsOverQuotaAccountWhenAllowOverUsage(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got == nil || got.ID != "over" {
		t.Fatalf("expected over-quota account to remain routable when allowOverUsage=true, got %#v", got)
	}
}

func TestReloadDropsOverQuotaAccountWhenAllowOverUsageDisabled(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got != nil {
		t.Fatalf("expected over-quota account to be dropped, got %q", got.ID)
	}
}

// ---------------------------------------------------------------------------
// Local failover routing extensions
// ---------------------------------------------------------------------------

func initPoolTestConfig(t *testing.T) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
}

func TestGetNextAllowsExpiredAccountWithRefreshToken(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:           "refreshable",
		AccessToken:  "expired-access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	}

	p.accounts = []config.Account{account}

	got := p.GetNext()
	if got == nil {
		t.Fatalf("expected expired account with refresh token to be routable for refresh")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

func TestGetNextSkipsExpiredAccountWithoutRefreshToken(t *testing.T) {
	p := &AccountPool{}
	p.accounts = []config.Account{
		{ID: "expired", AccessToken: "expired-access-token", ExpiresAt: time.Now().Add(-time.Minute).Unix()},
	}

	if got := p.GetNext(); got != nil {
		t.Fatalf("expected expired account without refresh token to be skipped, got %#v", got)
	}
}

func TestAvailableCountMatchesRealRoutingConstraints(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateAllowOverUsage(false); err != nil {
		t.Fatalf("disable global over-usage: %v", err)
	}

	now := time.Now()
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "ok", AccessToken: "token", ExpiresAt: now.Add(10 * time.Minute).Unix()},
			{ID: "cooldown", AccessToken: "token", ExpiresAt: now.Add(10 * time.Minute).Unix()},
			{ID: "expired", AccessToken: "token", ExpiresAt: now.Add(30 * time.Second).Unix()},
			{ID: "refreshable", AccessToken: "token", RefreshToken: "refresh-token", ExpiresAt: now.Add(30 * time.Second).Unix()},
			{ID: "over", AccessToken: "token", ExpiresAt: now.Add(10 * time.Minute).Unix(), UsageCurrent: 10, UsageLimit: 10},
		},
		cooldowns: map[string]time.Time{"cooldown": now.Add(time.Minute)},
	}

	if got := p.AvailableCount(); got != 2 {
		t.Fatalf("expected 2 available or refreshable accounts, got %d", got)
	}
}

func TestAvailableCountSkipsExpiredAccountWithoutRefreshToken(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateAllowOverUsage(false); err != nil {
		t.Fatalf("disable global over-usage: %v", err)
	}

	now := time.Now()
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "expired", AccessToken: "token", ExpiresAt: now.Add(-time.Minute).Unix()},
		},
	}

	if got := p.AvailableCount(); got != 0 {
		t.Fatalf("expected expired account without refresh token to be unavailable, got %d", got)
	}
}

func TestHealthSnapshotsKeepRefreshableExpiredAccountsRoutable(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateAllowOverUsage(false); err != nil {
		t.Fatalf("disable global over-usage: %v", err)
	}

	now := time.Now()
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "refreshable", AccessToken: "token", RefreshToken: "refresh-token", ExpiresAt: now.Add(-time.Minute).Unix()},
			{ID: "expired", AccessToken: "token", ExpiresAt: now.Add(-time.Minute).Unix()},
		},
		errorCounts: make(map[string]int),
		requestLog:  make(map[string][]requestEvent),
		lastErrorAt: make(map[string]time.Time),
	}

	snapshots := p.GetHealthSnapshots()
	if !snapshots["refreshable"].CanRoute {
		t.Fatalf("expected expired account with refresh token to remain routable for refresh")
	}
	if snapshots["expired"].CanRoute {
		t.Fatalf("expected expired account without refresh token to be non-routable")
	}
}
