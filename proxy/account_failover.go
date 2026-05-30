package proxy

import (
	"kiro-go/config"
	"kiro-go/logger"
	"strings"
	"time"
)

const maxAccountRetryAttempts = 3

func isSuspicious429ErrorMessage(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "429") && classifyKiro429Body(msg) == "suspicious_temporary_limits"
}

func isTransient429ErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "429") && !strings.Contains(lower, "too many requests") {
		return false
	}
	return classifyKiro429Body(msg) != "suspicious_temporary_limits"
}

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "quota")
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "refresh failed: 401") ||
		strings.Contains(msg, "refresh failed: 403") ||
		strings.Contains(msg, "bad credentials") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case isSuspicious429ErrorMessage(errMsg):
		h.pool.QuarantineAccount429(account.ID)
	case isTransient429ErrorMessage(errMsg):
		h.pool.RecordTransient429(account.ID)
		logger.Warnf("[AccountFailover] Transient 429 for %s, keeping account enabled for retry", account.Email)
	case isQuotaErrorMessage(errMsg):
		h.pool.QuarantineAccount429(account.ID)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case isAuthErrorMessage(errMsg):
		h.disableAccount(account, "DISABLED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}

func (h *Handler) handleAccountTestFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isSuspicious429ErrorMessage(errMsg):
		h.pool.QuarantineAccount429(account.ID)
	case isTransient429ErrorMessage(errMsg):
		h.pool.RecordTransient429(account.ID)
		logger.Warnf("[AccountFailover] Manual test hit transient 429 for %s, keeping account enabled", account.Email)
	case isQuotaErrorMessage(errMsg):
		h.pool.QuarantineAccount429(account.ID)
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.disableAccount(account, "DISABLED", "Manual test failed: "+errMsg)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		h.disableAccount(account, "SUSPENDED", "No available Kiro profile")
	case isAuthErrorMessage(errMsg):
		h.disableAccount(account, "DISABLED", "Authentication failed - token invalid or expired")
	default:
		h.disableAccount(account, "DISABLED", "Manual test failed: "+errMsg)
	}
}
