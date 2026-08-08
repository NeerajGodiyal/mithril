package mcp

import (
	"math"
	"strconv"
	"strings"
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

func TestEnvParsersRejectMalformedValues(t *testing.T) {
	const key = "MITHRIL_MCP_TEST_PARSER"

	t.Run("uint", func(t *testing.T) {
		cases := map[string]uint64{
			"":                     7, // unset falls back
			"0":                    0, // zero is a legal threshold here
			"42":                   42,
			"18446744073709551615": math.MaxUint64,
			"18446744073709551616": 7, // overflows uint64
			"-1":                   7,
			"4.2":                  7,
			" 42":                  7, // ParseUint does not trim
			"42 ":                  7,
			"42abc":                7, // must not partially parse
			"0x2a":                 7,
			"abc":                  7,
		}
		for raw, want := range cases {
			t.Setenv(key, raw)
			if got := parseEnvUint(key, 7); got != want {
				t.Errorf("parseEnvUint(%q) = %d, want %d", raw, got, want)
			}
		}
	})

	t.Run("bool", func(t *testing.T) {
		cases := map[string]bool{
			"true": true, "TRUE": true, "True": true, "1": true, "t": true,
			"false": false, "FALSE": false, "0": false, "f": false,
			// Not accepted by strconv.ParseBool, so these take the default.
			"yes": true, "on": true, "enabled": true, "": true, " true": true,
		}
		for raw, want := range cases {
			t.Setenv(key, raw)
			if got := parseEnvBool(key, true); got != want {
				t.Errorf("parseEnvBool(%q, true) = %v, want %v", raw, got, want)
			}
		}
	})

	t.Run("positive int", func(t *testing.T) {
		cases := map[string]int{
			"": 9, "5": 5, "0": 9, "-3": 9, "abc": 9, "5.5": 9, "5x": 9,
		}
		for raw, want := range cases {
			t.Setenv(key, raw)
			if got := parseEnvPositiveInt(key, 9); got != want {
				t.Errorf("parseEnvPositiveInt(%q) = %d, want %d", raw, got, want)
			}
		}
	})

	t.Run("positive float", func(t *testing.T) {
		cases := map[string]float64{
			"": 2.5, "1.5": 1.5, "0": 2.5, "-1": 2.5, "abc": 2.5,
			"NaN": 2.5, "Inf": 2.5, "+Inf": 2.5, "-Inf": 2.5,
		}
		for raw, want := range cases {
			t.Setenv(key, raw)
			if got := parseEnvPositiveFloat(key, 2.5); got != want {
				t.Errorf("parseEnvPositiveFloat(%q) = %v, want %v", raw, got, want)
			}
		}
	})
}

func TestControlToggleFailsClosed(t *testing.T) {
	for _, raw := range []string{"yes", "on", "enabled", "TRUE ", "1 ", "maybe", "-1"} {
		t.Setenv("MITHRIL_MCP_CONTROL_ENABLED", raw)
		if ConfigFromEnv().ControlEnabled {
			t.Errorf("MITHRIL_MCP_CONTROL_ENABLED=%q enabled service control; a value ParseBool rejects must fail closed", raw)
		}
	}
	// The accepted spellings must still work, or the toggle is unusable.
	for _, raw := range []string{"true", "TRUE", "1", "t", "True"} {
		t.Setenv("MITHRIL_MCP_CONTROL_ENABLED", raw)
		if !ConfigFromEnv().ControlEnabled {
			t.Errorf("MITHRIL_MCP_CONTROL_ENABLED=%q did not enable service control", raw)
		}
	}
}

func TestToolCallTimeoutClampsBeforeScaling(t *testing.T) {
	const key = "MITHRIL_MCP_TOOL_TIMEOUT_SECONDS"
	maxSecs := int64(MaxToolCallTimeout / time.Second)

	cases := map[string]time.Duration{
		"":                               DefaultToolCallTimeout, // unset
		"0":                              DefaultToolCallTimeout, // rejected as non-positive
		"-1":                             DefaultToolCallTimeout,
		"abc":                            DefaultToolCallTimeout,
		"1":                              time.Second,
		"90":                             90 * time.Second,
		strconv.FormatInt(maxSecs, 10):   MaxToolCallTimeout,
		strconv.FormatInt(maxSecs+1, 10): MaxToolCallTimeout, // clamped
		"999999999":                      MaxToolCallTimeout,
		"9223372036854775807":            MaxToolCallTimeout,     // math.MaxInt64 seconds
		"18446744073709551616":           DefaultToolCallTimeout, // unparseable, not clamped
	}

	for raw, want := range cases {
		t.Setenv(key, raw)
		got := parseEnvToolCallTimeout(key)
		if got != want {
			t.Errorf("parseEnvToolCallTimeout(%q) = %v, want %v", raw, got, want)
		}
		// The property that matters regardless of the exact value: a deadline
		// must always be positive and never exceed the declared maximum.
		if got <= 0 {
			t.Errorf("parseEnvToolCallTimeout(%q) = %v, which would abort every call", raw, got)
		}
		if got > MaxToolCallTimeout {
			t.Errorf("parseEnvToolCallTimeout(%q) = %v, above the %v maximum", raw, got, MaxToolCallTimeout)
		}
	}
}

func FuzzToolCallTimeoutAlwaysUsable(f *testing.F) {
	for _, seed := range []string{"", "0", "-1", "90", "600", "601", "9223372036854775807", "abc", "1e9", " 90 ", "+90"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		// The OS rejects NUL inside an environment value, so such a string can
		// never reach the parser and t.Setenv would fail the test on it.
		if strings.ContainsRune(raw, 0) {
			t.Skip()
		}
		t.Setenv("MITHRIL_MCP_TOOL_TIMEOUT_SECONDS", raw)
		got := parseEnvToolCallTimeout("MITHRIL_MCP_TOOL_TIMEOUT_SECONDS")
		if got <= 0 || got > MaxToolCallTimeout {
			t.Fatalf("env %q produced unusable tool deadline %v", raw, got)
		}
	})
}
