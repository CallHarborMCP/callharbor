package callpolicy

import (
	"math"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

func i64(v int64) *int64 { return &v }

func TestCheckDestinationDenyWinsOverAllow(t *testing.T) {
	e := Effective{
		DenyPrefixes: []string{"+1415"},
		AllowGroups:  [][]string{{"+1"}},
	}
	if got := CheckDestination(e, "+14155550111"); got != ReasonDestinationDenied {
		t.Fatalf("got %q, want %s", got, ReasonDestinationDenied)
	}
}

func TestCheckDestinationEveryAllowGroupMustMatch(t *testing.T) {
	e := Effective{AllowGroups: [][]string{{"+1"}, {"+44"}}}
	if got := CheckDestination(e, "+14155550111"); got != ReasonDestinationNotAllowed {
		t.Fatalf("got %q, want %s", got, ReasonDestinationNotAllowed)
	}
	e = Effective{AllowGroups: [][]string{{"+1"}, {"+1415"}}}
	if got := CheckDestination(e, "+14155550111"); got != "" {
		t.Fatalf("got %q, want allowed", got)
	}
}

func TestCheckDestinationNoListsAllowsEverything(t *testing.T) {
	if got := CheckDestination(Effective{}, "+14155550111"); got != "" {
		t.Fatalf("got %q, want allowed", got)
	}
}

// TestCheckDestinationIsPrefixNotSubstring: a prefix matches only at the start,
// so +1415 does not deny a number that merely contains it.
func TestCheckDestinationIsPrefixNotSubstring(t *testing.T) {
	e := Effective{DenyPrefixes: []string{"+1415"}}
	if got := CheckDestination(e, "+4415550111"); got != "" {
		t.Fatalf("substring match denied: %q", got)
	}
}

func TestResolveNeverLoosensInstallation(t *testing.T) {
	install := config.CallPolicy{
		UserMaxConcurrentCalls: 3,
		UserMaxCallsPerHour:    30,
		MaxCallDurationSeconds: 300,
		DenyPrefixes:           []string{"+1900"},
		AllowPrefixes:          []string{"+1"},
		MaxConcurrentCalls:     10,
	}
	loose := storage.UserCallPolicy{
		MaxConcurrentCalls: i64(50),
		MaxCallsPerHour:    i64(9999),
		MaxDurationSeconds: i64(7200),
		DenyPrefixes:       []string{"+1415"},
	}
	e := Resolve(install, loose)
	if e.MaxConcurrentCalls != 3 || e.MaxCallsPerHour != 30 || e.MaxDurationSeconds != 300 {
		t.Fatalf("limits loosened: %+v", e)
	}
	if e.GlobalMaxConcurrent != 10 {
		t.Fatalf("global = %d, want 10", e.GlobalMaxConcurrent)
	}
	if len(e.DenyPrefixes) != 2 || e.DenyPrefixes[0] != "+1900" || e.DenyPrefixes[1] != "+1415" {
		t.Fatalf("deny union = %v", e.DenyPrefixes)
	}
}

func TestResolveTightensInstallation(t *testing.T) {
	install := config.CallPolicy{UserMaxConcurrentCalls: 3, UserMaxCallsPerHour: 30, MaxCallDurationSeconds: 7200}
	e := Resolve(install, storage.UserCallPolicy{MaxConcurrentCalls: i64(1), MaxCallsPerHour: i64(5), MaxDurationSeconds: i64(60)})
	if e.MaxConcurrentCalls != 1 || e.MaxCallsPerHour != 5 || e.MaxDurationSeconds != 60 {
		t.Fatalf("tightened = %+v", e)
	}
}

// TestResolveNilOverrideInherits: an unset override keeps the installation
// value, it does not become zero (which would mean no limit).
func TestResolveNilOverrideInherits(t *testing.T) {
	install := config.CallPolicy{UserMaxConcurrentCalls: 3, UserMaxCallsPerHour: 30, MaxCallDurationSeconds: 300}
	e := Resolve(install, storage.UserCallPolicy{})
	if e.MaxConcurrentCalls != 3 || e.MaxCallsPerHour != 30 || e.MaxDurationSeconds != 300 {
		t.Fatalf("inherit = %+v", e)
	}
}

func TestResolveDurationDefaultsToSchemaMaximum(t *testing.T) {
	e := Resolve(config.CallPolicy{}, storage.UserCallPolicy{})
	if e.MaxDurationSeconds != SchemaMaxDurationSeconds {
		t.Fatalf("duration = %d, want %d", e.MaxDurationSeconds, SchemaMaxDurationSeconds)
	}
}

func TestResolveCostLimitTakesStricter(t *testing.T) {
	e := Resolve(config.CallPolicy{MaxCallCostMicros: i64(2_000_000)}, storage.UserCallPolicy{MaxCostMicros: i64(500_000)})
	if e.MaxCostMicros == nil || *e.MaxCostMicros != 500_000 {
		t.Fatalf("cost limit = %v, want 500000", e.MaxCostMicros)
	}
	e = Resolve(config.CallPolicy{}, storage.UserCallPolicy{MaxCostMicros: i64(7)})
	if e.MaxCostMicros == nil || *e.MaxCostMicros != 7 {
		t.Fatalf("owner-only cost limit = %v, want 7", e.MaxCostMicros)
	}
}

func TestEstimateMicrosRoundsPartialMinutesUp(t *testing.T) {
	cases := []struct {
		rate int64
		secs int
		want int64
	}{
		{250_000, 1, 250_000},
		{250_000, 60, 250_000},
		{250_000, 61, 500_000},
		{250_000, 7200, 30_000_000},
		{0, 600, 0},
	}
	for _, c := range cases {
		got, ok := EstimateMicros(c.rate, c.secs)
		if !ok || got != c.want {
			t.Fatalf("EstimateMicros(%d, %d) = %d, %v; want %d", c.rate, c.secs, got, ok, c.want)
		}
	}
}

// TestEstimateMicrosOverflowIsRefused: a product beyond int64 is reported, not
// wrapped to a small or negative amount.
func TestEstimateMicrosOverflowIsRefused(t *testing.T) {
	if _, ok := EstimateMicros(math.MaxInt64/2, 7200); ok {
		t.Fatal("overflowing estimate reported ok")
	}
}

func TestValidatePrefixesNamesFirstBadEntry(t *testing.T) {
	if err := ValidatePrefixes([]string{"+1415", "+44"}); err != nil {
		t.Fatalf("valid prefixes: %v", err)
	}
	for _, bad := range []string{"1415", "+0123", "+1 415", "+", "+1234567890123456"} {
		if err := ValidatePrefixes([]string{"+1415", bad}); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
