package mcp

import (
	"math"
	"testing"
	"time"
)

var admissionEnvKeys = []string{
	"MITHRIL_MCP_PROFILE",
	"MITHRIL_MCP_MAX_CONCURRENT",
	"MITHRIL_MCP_RATE_PER_SECOND",
	"MITHRIL_MCP_RATE_BURST",
	"MITHRIL_MCP_OUTPUT_BUDGET_BYTES",
	"MITHRIL_REPLAY_P99_WARN_MS",
	"MITHRIL_MCP_APPROVAL_TTL_SECONDS",
}

func clearAdmissionEnv(t *testing.T) {
	t.Helper()
	for _, key := range admissionEnvKeys {
		t.Setenv(key, "")
	}
}

func TestConfigFromEnvAdmissionDefaults(t *testing.T) {
	clearAdmissionEnv(t)
	cfg := ConfigFromEnv()
	if cfg.Profile != ProfileMonitor {
		t.Fatalf("Profile = %q, want %q", cfg.Profile, ProfileMonitor)
	}
	if cfg.MaxConcurrent != DefaultMaxConcurrent ||
		cfg.RatePerSecond != DefaultRatePerSecond ||
		cfg.RateBurst != DefaultRateBurst ||
		cfg.OutputBudgetBytes != DefaultOutputBudgetBytes ||
		cfg.ReplayP99WarnMs != DefaultReplayP99WarnMs {
		t.Fatalf("unexpected admission defaults: %+v", cfg)
	}
}

func TestConfigFromEnvAdmissionOverridesAndClamp(t *testing.T) {
	clearAdmissionEnv(t)
	t.Setenv("MITHRIL_MCP_PROFILE", " diagnostic ")
	t.Setenv("MITHRIL_MCP_MAX_CONCURRENT", "7")
	t.Setenv("MITHRIL_MCP_RATE_PER_SECOND", "2.5")
	t.Setenv("MITHRIL_MCP_RATE_BURST", "3")
	t.Setenv("MITHRIL_MCP_OUTPUT_BUDGET_BYTES", "999999999")
	t.Setenv("MITHRIL_REPLAY_P99_WARN_MS", "321.5")

	cfg := ConfigFromEnv()
	if cfg.Profile != ProfileDiagnostic || cfg.MaxConcurrent != 7 || cfg.RatePerSecond != 2.5 || cfg.RateBurst != 3 || cfg.ReplayP99WarnMs != 321.5 {
		t.Fatalf("environment overrides not applied: %+v", cfg)
	}
	if cfg.OutputBudgetBytes != MaxOutputBudgetBytes {
		t.Fatalf("OutputBudgetBytes = %d, want hard maximum %d", cfg.OutputBudgetBytes, MaxOutputBudgetBytes)
	}
}

func TestConfigFromEnvClampsOutputBudgetToWireSafeMinimum(t *testing.T) {
	clearAdmissionEnv(t)
	t.Setenv("MITHRIL_MCP_OUTPUT_BUDGET_BYTES", "1")
	if got := ConfigFromEnv().OutputBudgetBytes; got != MinOutputBudgetBytes {
		t.Fatalf("OutputBudgetBytes = %d, want hard minimum %d", got, MinOutputBudgetBytes)
	}
}

func TestConfigFromEnvRejectsZeroAndInvalidLimits(t *testing.T) {
	clearAdmissionEnv(t)
	t.Setenv("MITHRIL_MCP_PROFILE", "all-powerful")
	t.Setenv("MITHRIL_MCP_MAX_CONCURRENT", "0")
	t.Setenv("MITHRIL_MCP_RATE_PER_SECOND", "NaN")
	t.Setenv("MITHRIL_MCP_RATE_BURST", "-1")
	t.Setenv("MITHRIL_MCP_OUTPUT_BUDGET_BYTES", "not-a-number")
	t.Setenv("MITHRIL_REPLAY_P99_WARN_MS", "NaN")

	cfg := ConfigFromEnv()
	if cfg.Profile != ProfileMonitor ||
		cfg.MaxConcurrent != DefaultMaxConcurrent ||
		cfg.RatePerSecond != DefaultRatePerSecond ||
		cfg.RateBurst != DefaultRateBurst ||
		cfg.OutputBudgetBytes != DefaultOutputBudgetBytes ||
		cfg.ReplayP99WarnMs != DefaultReplayP99WarnMs {
		t.Fatalf("invalid values did not fall back safely: %+v", cfg)
	}
}

func TestConfigNormalizedUsesSafeDefaultsAndHardBudget(t *testing.T) {
	cfg := (Config{
		Profile: " Diagnostic ", OutputBudgetBytes: MaxOutputBudgetBytes + 1,
		MaxConcurrent: MaxConcurrentLimit + 1, RatePerSecond: MaxRatePerSecond + 1, RateBurst: MaxRateBurst + 1,
		ReplayP99WarnMs: math.Inf(1),
	}).normalized()
	if cfg.Profile != ProfileDiagnostic || cfg.MaxConcurrent == 0 || cfg.RatePerSecond == 0 || cfg.RateBurst == 0 {
		t.Fatalf("zero-value config was not normalized safely: %+v", cfg)
	}
	if cfg.OutputBudgetBytes != MaxOutputBudgetBytes {
		t.Fatalf("OutputBudgetBytes = %d, want %d", cfg.OutputBudgetBytes, MaxOutputBudgetBytes)
	}
	if cfg.MaxConcurrent != MaxConcurrentLimit || cfg.RatePerSecond != MaxRatePerSecond || cfg.RateBurst != MaxRateBurst {
		t.Fatalf("admission hard limits were not applied: %+v", cfg)
	}
	if cfg.ReplayP99WarnMs != DefaultReplayP99WarnMs {
		t.Fatalf("ReplayP99WarnMs = %v, want finite positive default %v", cfg.ReplayP99WarnMs, DefaultReplayP99WarnMs)
	}
}

func TestConfigNormalizedClampsPositiveOutputBudgetToMinimum(t *testing.T) {
	if got := (Config{OutputBudgetBytes: 1}).normalized().OutputBudgetBytes; got != MinOutputBudgetBytes {
		t.Fatalf("OutputBudgetBytes = %d, want hard minimum %d", got, MinOutputBudgetBytes)
	}
}

func TestConfigPreservesExplicitApprovalTTLForValidation(t *testing.T) {
	clearAdmissionEnv(t)
	if got := (Config{}).normalized().ApprovalTTLSeconds; got != DefaultApprovalTTLSeconds {
		t.Fatalf("zero approval TTL = %d, want default %d", got, DefaultApprovalTTLSeconds)
	}
	if got := (Config{ApprovalTTLSeconds: MinApprovalTTLSeconds - 1}).normalized().ApprovalTTLSeconds; got != MinApprovalTTLSeconds-1 {
		t.Fatalf("invalid explicit approval TTL was silently widened to %d", got)
	}
	t.Setenv("MITHRIL_MCP_APPROVAL_TTL_SECONDS", "10")
	if got := ConfigFromEnv().ApprovalTTLSeconds; got != 10 {
		t.Fatalf("environment approval TTL was silently widened to %d", got)
	}
	t.Setenv("MITHRIL_MCP_APPROVAL_TTL_SECONDS", "0")
	if got := ConfigFromEnv().ApprovalTTLSeconds; got <= MaxApprovalTTLSeconds {
		t.Fatalf("zero environment approval TTL became an accepted value: %d", got)
	}
	t.Setenv("MITHRIL_MCP_APPROVAL_TTL_SECONDS", "invalid")
	if got := ConfigFromEnv().ApprovalTTLSeconds; got <= MaxApprovalTTLSeconds {
		t.Fatalf("invalid environment approval TTL became an accepted value: %d", got)
	}
}

func TestConfigFromEnvPreservesExplicitlyDisabledEndpoints(t *testing.T) {
	t.Setenv("MITHRIL_METRICS_URL", "")
	t.Setenv("MITHRIL_RPC_URL", "")
	cfg := ConfigFromEnv()
	if cfg.MetricsURL != "" || cfg.RPCURL != "" {
		t.Fatalf("explicitly disabled endpoints were defaulted: %+v", cfg)
	}
}

func TestParseBlockSource(t *testing.T) {
	for _, source := range []string{"rpc", " lightbringer ", "TURBINE"} {
		if _, err := ParseBlockSource(source); err != nil {
			t.Errorf("valid block source %q: %v", source, err)
		}
	}
	for _, source := range []string{"", "lightbrigner", "all"} {
		if _, err := ParseBlockSource(source); err == nil {
			t.Errorf("invalid block source %q was accepted", source)
		}
	}
}

func TestToolCallTimeoutFromEnv(t *testing.T) {
	t.Setenv("MITHRIL_MCP_TOOL_TIMEOUT_SECONDS", "45")
	if got := ConfigFromEnv().normalized().ToolCallTimeout; got != 45*time.Second {
		t.Errorf("ToolCallTimeout = %v, want 45s", got)
	}
	t.Setenv("MITHRIL_MCP_TOOL_TIMEOUT_SECONDS", "0")
	if got := ConfigFromEnv().normalized().ToolCallTimeout; got != DefaultToolCallTimeout {
		t.Errorf("invalid value should fall back to default, got %v", got)
	}
	if got := (Config{ToolCallTimeout: MaxToolCallTimeout + time.Second}).normalized().ToolCallTimeout; got != DefaultToolCallTimeout {
		t.Errorf("over-max should fall back to default, got %v", got)
	}
}
