// Package config provides configuration management for Kiro API Proxy.
//
// This package handles persistent storage and retrieval of:
//   - Account credentials and authentication tokens
//   - Server settings (port, host, API keys)
//   - Usage statistics and metrics
//   - Thinking mode configuration for AI responses
//
// All configuration is stored in a JSON file with thread-safe access
// via read-write mutex protection.
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// GenerateMachineId generates a UUID v4 format machine identifier.
// This ID is used to uniquely identify the proxy instance in Kiro API requests,
// helping with request tracking and rate limiting on the server side.
func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // 版本 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

var (
	cfg     *Config
	cfgLock sync.RWMutex
	cfgPath string
)

const (
	autoQuarantineSuspicious429Reason = "AUTO_QUARANTINE_SUSPICIOUS_429"
	autoQuarantineDuration            = time.Hour
)

func defaultRoutingConcurrencyConfig() RoutingConcurrencyConfig {
	return RoutingConcurrencyConfig{
		Enabled:                 false,
		GlobalMaxConcurrent:     0,
		GlobalQueueSize:         100,
		GlobalQueueTimeoutMs:    30000,
		PerAccountMaxConcurrent: 1,
		PerAccountMinIntervalMs: 0,
		StickyAccount:           true,
		OverflowToOtherAccounts: true,
	}
}

func normalizeRoutingConcurrencyConfig(in RoutingConcurrencyConfig) RoutingConcurrencyConfig {
	def := defaultRoutingConcurrencyConfig()
	isEmpty := in == (RoutingConcurrencyConfig{})
	out := in
	out.Enabled = in.Enabled
	if out.GlobalMaxConcurrent < 0 {
		out.GlobalMaxConcurrent = 0
	}
	if isEmpty || (!out.Enabled && out.GlobalQueueSize == 0) {
		out.GlobalQueueSize = def.GlobalQueueSize
	} else if out.GlobalQueueSize < 0 {
		out.GlobalQueueSize = 0
	}
	if out.GlobalQueueTimeoutMs <= 0 {
		out.GlobalQueueTimeoutMs = def.GlobalQueueTimeoutMs
	}
	if out.PerAccountMaxConcurrent <= 0 {
		out.PerAccountMaxConcurrent = def.PerAccountMaxConcurrent
	}
	if out.PerAccountMinIntervalMs < 0 {
		out.PerAccountMinIntervalMs = 0
	}
	// Zero-value bools from older config should still get the intended defaults
	// unless the user explicitly disables them through the settings API. The API
	// always writes both booleans, so this only affects first-time migration.
	if !out.StickyAccount && isEmpty {
		out.StickyAccount = def.StickyAccount
	}
	if !out.OverflowToOtherAccounts && isEmpty {
		out.OverflowToOtherAccounts = def.OverflowToOtherAccounts
	}
	return out
}

// Init initializes the configuration system with the specified file path.
// If the file doesn't exist, a default configuration is created.
func Init(path string) error {
	cfgPath = path
	return Load()
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create default configuration.
			// Binds to 0.0.0.0 by default for Docker/container compatibility.
			cfg = &Config{
				Password:           "changeme",
				Port:               8080,
				Host:               "0.0.0.0",
				RequireApiKey:      false,
				Accounts:           []Account{},
				RoutingConcurrency: defaultRoutingConcurrencyConfig(),
			}
			return saveLocked()
		}
		return err
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	cfg = &c
	cfg.RoutingConcurrency = normalizeRoutingConcurrencyConfig(cfg.RoutingConcurrency)

	// Migration: if a legacy single ApiKey is present and the new ApiKeys list is empty,
	// promote it into the new structure. The migrated entry inherits the legacy
	// RequireApiKey state — if the legacy deployment was public (RequireApiKey=false),
	// we mark the entry disabled so it doesn't accidentally start enforcing auth.
	// Operators can flip it on later from the admin UI. The legacy field is kept
	// for backward compatibility when reading older config files.
	if cfg.ApiKey != "" && len(cfg.ApiKeys) == 0 {
		cfg.ApiKeys = append(cfg.ApiKeys, ApiKeyEntry{
			ID:        newUUID(),
			Name:      "legacy",
			Key:       cfg.ApiKey,
			Enabled:   cfg.RequireApiKey,
			Migrated:  true,
			CreatedAt: time.Now().Unix(),
		})
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: per-account AllowOverage → OverageStatus.
	// Pre-Overages-switch deployments stored `allowOverage: true` to mean "keep
	// dispatching when quota is exhausted". The new model reads OverageStatus
	// from the upstream AWS Q switch instead. To avoid silently disabling
	// previously-allowed accounts on first launch, treat allowOverage=true as
	// OverageStatus="ENABLED" (operators can refresh from AWS later). The
	// legacy field is then cleared so future saves don't re-emit it.
	overageMigrated := false
	for i := range cfg.Accounts {
		if cfg.Accounts[i].LegacyAllowOverage {
			if cfg.Accounts[i].OverageStatus == "" {
				cfg.Accounts[i].OverageStatus = "ENABLED"
			}
			cfg.Accounts[i].LegacyAllowOverage = false
			overageMigrated = true
		}
	}
	if overageMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

// saveLocked persists cfg to disk. Caller MUST already hold cfgLock.
// This is identical to Save() (which does not take the lock either) but is named
// distinctly so call sites that already hold cfgLock are explicit about it.
func saveLocked() error {
	return Save()
}

// newUUID returns a UUID v4 string. Defined here to avoid pulling extra deps in this file.
func newUUID() string {
	return GenerateMachineId()
}

// Save persists the current configuration to the JSON file.
// Uses indented formatting for human readability.
func Save() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, data, 0600)
}

// GetConfigDir returns the directory containing the config JSON file.
// Useful for sibling state (e.g. stored Responses, caches) that should live
// alongside the configuration file.
func GetConfigDir() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfgPath == "" {
		return "."
	}
	dir := cfgPath
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			return dir[:i]
		}
	}
	return "."
}

func Get() *Config {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg
}
