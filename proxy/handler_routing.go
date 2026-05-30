package proxy

import (
	"context"
	"errors"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/pool"
	"time"
)

func (h *Handler) acquireRouteAccount(ctx context.Context, model string, excluded map[string]bool, apiKeyID string) (*config.Account, func(), error) {
	return h.pool.AcquireForModel(ctx, model, excluded, apiKeyID)
}

func isRoutingLimitError(err error) bool {
	return errors.Is(err, pool.ErrRoutingQueueFull) || errors.Is(err, pool.ErrRoutingQueueTimeout)
}

func routingErrorMessage(err error) string {
	if errors.Is(err, pool.ErrRoutingQueueFull) {
		return "Routing queue full"
	}
	if errors.Is(err, pool.ErrRoutingQueueTimeout) {
		return "Routing queue timeout"
	}
	if err != nil {
		return err.Error()
	}
	return "Routing unavailable"
}

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
		return nil
	}

	h.tokenRefreshMu.Lock()
	defer h.tokenRefreshMu.Unlock()

	// Another concurrent request may have refreshed this account while we waited.
	if latest := h.pool.GetByID(account.ID); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
		if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
	}

	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshToken(account)
	if err != nil {
		return err
	}

	// 更新内存
	h.pool.UpdateToken(account.ID, accessToken, refreshToken, expiresAt)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
		config.UpdateAccountProfileArn(account.ID, profileArn)
	}

	// 持久化
	config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt)

	return nil
}
