package handler

import (
	"strings"
	"testing"
	"time"
)

func TestRejectCredentialFields(t *testing.T) {
	ok := []byte(`{"provider":"claude","collected_at":"2026-09-22T00:00:00Z","windows":[{"id":"session","percent_used":12}]}`)
	if err := rejectCredentialFields(ok); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"provider":"cursor","access_token":"secret"}`,
		`{"cookie":"WorkosCursorSessionToken=abc::def"}`,
		`{"provider":"codex","windows":[{"id":"primary","percent_used":1,"note":"Bearer abc"}]}`,
		`{"plan_name":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.signature-padding"}`,
	} {
		if err := rejectCredentialFields([]byte(body)); err == nil {
			t.Fatalf("accepted credential body %s", body)
		}
	}
}

func TestNormalizeProviderUsageReport(t *testing.T) {
	collected := time.Date(2026, 9, 22, 1, 2, 3, 0, time.UTC)
	reset := collected.Add(time.Hour)
	got, err := normalizeProviderUsageReport(providerUsageReport{
		Provider:    "Claude",
		PlanName:    "Max",
		CollectedAt: collected,
		Windows: []providerUsageWindowReport{
			{ID: "session", PercentUsed: 38, ResetsAt: &reset},
			{ID: "weekly_all", PercentUsed: 4},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "claude" || got.PlanName != "Max" || len(got.Windows) != 2 || got.ReasonCode != "" {
		t.Fatalf("normalized = %+v", got)
	}

	empty, err := normalizeProviderUsageReport(providerUsageReport{
		Provider:    "codex",
		CollectedAt: collected,
		ReasonCode:  "api_key_only",
	})
	if err != nil || empty.ReasonCode != "api_key_only" || len(empty.Windows) != 0 {
		t.Fatalf("empty = %+v err=%v", empty, err)
	}

	if _, err := normalizeProviderUsageReport(providerUsageReport{Provider: "claude"}); err == nil {
		t.Fatal("missing collected_at accepted")
	}
	if _, err := normalizeProviderUsageReport(providerUsageReport{
		Provider:    "nope",
		CollectedAt: collected,
	}); err == nil {
		t.Fatal("unknown provider accepted")
	}
	if _, err := normalizeProviderUsageReport(providerUsageReport{
		Provider:    "cursor",
		CollectedAt: collected,
		ReasonCode:  "drop table",
	}); err == nil {
		t.Fatal("unknown reason accepted")
	}
	if _, err := normalizeProviderUsageReport(providerUsageReport{
		Provider:    "cursor",
		CollectedAt: collected,
		Windows:     []providerUsageWindowReport{{ID: "Auto", PercentUsed: 1}},
	}); err == nil {
		t.Fatal("non-lowercase window id accepted")
	}
}

func TestProviderUsageErrorDoesNotEchoSecrets(t *testing.T) {
	err := rejectCredentialFields([]byte(`{"access_token":"super-secret-token"}`))
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatal("error echoed the token")
	}
}
