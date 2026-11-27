package config

import (
	"strings"
	"testing"
)

func TestLoadCallPolicyDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	p := cfg.CallPolicy
	if p.MaxConcurrentCalls != 10 || p.UserMaxConcurrentCalls != 3 || p.UserMaxCallsPerHour != 30 {
		t.Errorf("limits = %d/%d/%d, want 10/3/30", p.MaxConcurrentCalls, p.UserMaxConcurrentCalls, p.UserMaxCallsPerHour)
	}
	if p.MaxCallDurationSeconds != 7200 || p.RingTimeoutSeconds != 30 {
		t.Errorf("durations = %d/%d, want 7200/30", p.MaxCallDurationSeconds, p.RingTimeoutSeconds)
	}
	if len(p.AllowPrefixes) != 0 || len(p.DenyPrefixes) != 0 {
		t.Errorf("prefix lists = %v / %v, want empty", p.AllowPrefixes, p.DenyPrefixes)
	}
	if p.MaxCallCostMicros != nil || p.CallCostEstimateMicrosPerMinute != nil {
		t.Error("cost settings must be unset by default; no tariff is invented")
	}
}

func TestLoadCallPolicyValidValues(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		EnvCallAllowPrefixes:         "+1415, +44 ",
		EnvCallDenyPrefixes:          "+1900",
		EnvMaxConcurrentCalls:        "50",
		EnvUserMaxConcurrentCalls:    "5",
		EnvUserMaxCallsPerHour:       "100",
		EnvMaxCallDurationSeconds:    "600",
		EnvMaxCallCost:               "1.5",
		EnvCallCostEstimatePerMinute: "0.025000",
		EnvCallCostCurrency:          "EUR",
		EnvRingTimeoutSeconds:        "45",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	p := cfg.CallPolicy
	if strings.Join(p.AllowPrefixes, ",") != "+1415,+44" || strings.Join(p.DenyPrefixes, ",") != "+1900" {
		t.Errorf("prefixes = %v / %v", p.AllowPrefixes, p.DenyPrefixes)
	}
	if p.MaxConcurrentCalls != 50 || p.UserMaxConcurrentCalls != 5 || p.UserMaxCallsPerHour != 100 {
		t.Errorf("limits not applied: %+v", p)
	}
	if p.MaxCallDurationSeconds != 600 || p.RingTimeoutSeconds != 45 {
		t.Errorf("durations not applied: %+v", p)
	}
	if p.MaxCallCostMicros == nil || *p.MaxCallCostMicros != 1_500_000 {
		t.Errorf("max cost = %v, want 1500000 micros", p.MaxCallCostMicros)
	}
	if p.CallCostEstimateMicrosPerMinute == nil || *p.CallCostEstimateMicrosPerMinute != 25_000 {
		t.Errorf("estimate = %v, want 25000 micros", p.CallCostEstimateMicrosPerMinute)
	}
	if p.CallCostCurrency != "EUR" {
		t.Errorf("currency = %q", p.CallCostCurrency)
	}
}

func TestLoadCallPolicyRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]string
		wantKey string
	}{
		{"prefix without plus", map[string]string{EnvCallAllowPrefixes: "1415"}, EnvCallAllowPrefixes},
		{"prefix with letters", map[string]string{EnvCallDenyPrefixes: "+1-415"}, EnvCallDenyPrefixes},
		{"prefix too long", map[string]string{EnvCallDenyPrefixes: "+1234567890123456"}, EnvCallDenyPrefixes},
		{"empty prefix entry", map[string]string{EnvCallDenyPrefixes: "+1415,,+44"}, EnvCallDenyPrefixes},
		{"concurrency zero", map[string]string{EnvMaxConcurrentCalls: "0"}, EnvMaxConcurrentCalls},
		{"concurrency above cap", map[string]string{EnvMaxConcurrentCalls: "1001"}, EnvMaxConcurrentCalls},
		{"user concurrency not a number", map[string]string{EnvUserMaxConcurrentCalls: "three"}, EnvUserMaxConcurrentCalls},
		{"hourly rate above cap", map[string]string{EnvUserMaxCallsPerHour: "10001"}, EnvUserMaxCallsPerHour},
		{"duration below floor", map[string]string{EnvMaxCallDurationSeconds: "29"}, EnvMaxCallDurationSeconds},
		{"duration above schema max", map[string]string{EnvMaxCallDurationSeconds: "7201"}, EnvMaxCallDurationSeconds},
		{"ring timeout too short", map[string]string{EnvRingTimeoutSeconds: "4"}, EnvRingTimeoutSeconds},
		{"ring timeout too long", map[string]string{EnvRingTimeoutSeconds: "601"}, EnvRingTimeoutSeconds},
		{"cost not decimal", map[string]string{EnvMaxCallCost: "1,50", EnvCallCostCurrency: "USD"}, EnvMaxCallCost},
		{"cost scientific", map[string]string{EnvMaxCallCost: "1e3", EnvCallCostCurrency: "USD"}, EnvMaxCallCost},
		{"cost too many decimals", map[string]string{EnvMaxCallCost: "1.0000001", EnvCallCostCurrency: "USD"}, EnvMaxCallCost},
		{"cost zero", map[string]string{EnvMaxCallCost: "0", EnvCallCostCurrency: "USD"}, EnvMaxCallCost},
		{"cost set without currency", map[string]string{EnvMaxCallCost: "1.50"}, EnvCallCostCurrency},
		{"estimate set without currency", map[string]string{EnvCallCostEstimatePerMinute: "0.1"}, EnvCallCostCurrency},
		{"lowercase currency", map[string]string{EnvMaxCallCost: "1.50", EnvCallCostCurrency: "usd"}, EnvCallCostCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.values))
			if err == nil {
				t.Fatalf("Load() error = nil, want error mentioning %s", tt.wantKey)
			}
			if !strings.Contains(err.Error(), tt.wantKey) {
				t.Errorf("error = %q, want it to mention %s", err, tt.wantKey)
			}
		})
	}
}

func TestLoadCallPolicyReportsAllErrorsTogether(t *testing.T) {
	_, err := Load(env(map[string]string{
		EnvMaxConcurrentCalls: "0",
		EnvRingTimeoutSeconds: "1",
		EnvCallAllowPrefixes:  "bogus",
	}))
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	for _, key := range []string{EnvMaxConcurrentCalls, EnvRingTimeoutSeconds, EnvCallAllowPrefixes} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error = %q, want it to mention %s", err, key)
		}
	}
}

func TestParseMicrosIsExact(t *testing.T) {
	cases := map[string]int64{"1": 1_000_000, "0.5": 500_000, "1.50": 1_500_000, "0.000001": 1}
	for in, want := range cases {
		got, err := parseMicros(in)
		if err != nil || got != want {
			t.Errorf("parseMicros(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}
