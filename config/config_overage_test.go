package config

import (
	"path/filepath"
	"testing"
)

// TestClearAccountCurrentOverages verifies the fix for the bug where a reset
// subscription quota (e.g. 0 / 11,000) still showed stale overage points
// (e.g. 206 / 10,000). Clearing must zero CurrentOverages while preserving the
// OverageStatus switch and the cap/rate billing config.
func TestClearAccountCurrentOverages(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{
		ID:                "acc",
		Enabled:           true,
		OverageStatus:     "ENABLED",
		OverageCapability: "OVERAGE_CAPABLE",
		OverageCap:        10000,
		OverageRate:       1,
		CurrentOverages:   206,
		OverageCheckedAt:  100,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	if err := ClearAccountCurrentOverages("acc", 200); err != nil {
		t.Fatalf("clear overages: %v", err)
	}

	got := accountByID(t, "acc")
	if got.CurrentOverages != 0 {
		t.Fatalf("expected CurrentOverages cleared to 0, got %v", got.CurrentOverages)
	}
	// Billing config and switch must survive the clear.
	if got.OverageStatus != "ENABLED" {
		t.Fatalf("expected OverageStatus preserved, got %q", got.OverageStatus)
	}
	if got.OverageCapability != "OVERAGE_CAPABLE" {
		t.Fatalf("expected OverageCapability preserved, got %q", got.OverageCapability)
	}
	if got.OverageCap != 10000 {
		t.Fatalf("expected OverageCap preserved, got %v", got.OverageCap)
	}
	if got.OverageRate != 1 {
		t.Fatalf("expected OverageRate preserved, got %v", got.OverageRate)
	}
	if got.OverageCheckedAt != 200 {
		t.Fatalf("expected OverageCheckedAt updated to 200, got %v", got.OverageCheckedAt)
	}
}

// TestClearAccountCurrentOveragesNoopWhenAlreadyZero verifies the periodic
// refresh loop (which calls this every cycle for within-quota accounts) does
// not churn OverageCheckedAt / rewrite config when there is nothing to clear.
func TestClearAccountCurrentOveragesNoopWhenAlreadyZero(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{
		ID:               "acc",
		Enabled:          true,
		CurrentOverages:  0,
		OverageCheckedAt: 100,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	if err := ClearAccountCurrentOverages("acc", 999); err != nil {
		t.Fatalf("clear overages: %v", err)
	}

	got := accountByID(t, "acc")
	if got.CurrentOverages != 0 {
		t.Fatalf("expected CurrentOverages to stay 0, got %v", got.CurrentOverages)
	}
	// No-op path must not bump the checked-at timestamp.
	if got.OverageCheckedAt != 100 {
		t.Fatalf("expected OverageCheckedAt unchanged on no-op, got %v", got.OverageCheckedAt)
	}
}

func accountByID(t *testing.T, id string) Account {
	t.Helper()
	for _, a := range GetAccounts() {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("account %q not found", id)
	return Account{}
}
