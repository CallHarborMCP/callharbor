package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Call policy settings. They bound outbound calls for the whole installation.
// A per-user policy (set by an administrator) can only tighten them.
const (
	// EnvCallAllowPrefixes is a comma-separated list of E.164 prefixes. When it
	// is set, a destination must start with one of them. Unset allows any.
	EnvCallAllowPrefixes = "CALLHARBOR_CALL_ALLOW_PREFIXES"
	// EnvCallDenyPrefixes is a comma-separated list of E.164 prefixes that are
	// never dialed. Deny wins over allow.
	EnvCallDenyPrefixes = "CALLHARBOR_CALL_DENY_PREFIXES"
	// EnvMaxConcurrentCalls limits non-terminal calls across the installation.
	EnvMaxConcurrentCalls = "CALLHARBOR_MAX_CONCURRENT_CALLS"
	// EnvUserMaxConcurrentCalls is the default per-user limit on non-terminal calls.
	EnvUserMaxConcurrentCalls = "CALLHARBOR_USER_MAX_CONCURRENT_CALLS"
	// EnvUserMaxCallsPerHour is the default per-user count of calls created in
	// the trailing 60 minutes, whatever their status.
	EnvUserMaxCallsPerHour = "CALLHARBOR_USER_MAX_CALLS_PER_HOUR"
	// EnvMaxCallDurationSeconds is the installation maximum for one call.
	EnvMaxCallDurationSeconds = "CALLHARBOR_MAX_CALL_DURATION_SECONDS"
	// EnvMaxCallCost is an optional per-call cost limit, a decimal amount such
	// as 1.50 in CALLHARBOR_CALL_COST_CURRENCY. Unset means no cost limit.
	EnvMaxCallCost = "CALLHARBOR_MAX_CALL_COST"
	// EnvCallCostEstimatePerMinute is an operator-supplied rate used only to
	// estimate cost before a dial. It is never presented as a provider charge.
	EnvCallCostEstimatePerMinute = "CALLHARBOR_CALL_COST_ESTIMATE_PER_MINUTE"
	// EnvCallCostCurrency is the ISO 4217 code for the cost settings. It is
	// required when either cost setting is set.
	EnvCallCostCurrency = "CALLHARBOR_CALL_COST_CURRENCY"
	// EnvRingTimeoutSeconds is how long Telnyx rings before it gives up.
	EnvRingTimeoutSeconds = "CALLHARBOR_RING_TIMEOUT_SECONDS"

	DefaultMaxConcurrentCalls     = 10
	MinMaxConcurrentCalls         = 1
	MaxMaxConcurrentCalls         = 1000
	DefaultUserMaxConcurrentCalls = 3
	DefaultUserMaxCallsPerHour    = 30
	MinUserMaxCallsPerHour        = 1
	MaxUserMaxCallsPerHour        = 10000
	DefaultMaxCallDurationSeconds = 7200
	MinMaxCallDurationSeconds     = 30
	MaxMaxCallDurationSeconds     = 7200
	DefaultRingTimeoutSeconds     = 30
	MinRingTimeoutSeconds         = 5
	MaxRingTimeoutSeconds         = 600
	// MicrosPerUnit is the number of micro-units in one currency unit. Money is
	// stored and compared as int64 micro-units, never as floats.
	MicrosPerUnit = 1_000_000
)

// CallPolicy is the installation-wide call policy. Limits are plain integers,
// money is integer micro-units, and a nil cost pointer means "not configured".
type CallPolicy struct {
	AllowPrefixes                   []string
	DenyPrefixes                    []string
	MaxConcurrentCalls              int
	UserMaxConcurrentCalls          int
	UserMaxCallsPerHour             int
	MaxCallDurationSeconds          int
	MaxCallCostMicros               *int64
	CallCostEstimateMicrosPerMinute *int64
	CallCostCurrency                string
	RingTimeoutSeconds              int
}

var (
	e164PrefixPattern = regexp.MustCompile(`^\+[1-9][0-9]{0,14}$`)
	currencyPattern   = regexp.MustCompile(`^[A-Z]{3}$`)
	// decimalMoney allows up to 12 whole digits and 6 fractional digits, so the
	// micro-unit value always fits in int64.
	decimalMoney = regexp.MustCompile(`^([0-9]{1,12})(?:\.([0-9]{1,6}))?$`)
)

// loadCallPolicy reads the call policy settings. Every problem is returned, so
// one start reports all of them. Unset settings take their defaults.
func loadCallPolicy(getenv func(string) string) (CallPolicy, []error) {
	p := DefaultCallPolicy()
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	var err error
	p.AllowPrefixes, err = prefixSetting(getenv, EnvCallAllowPrefixes)
	collect(err)
	p.DenyPrefixes, err = prefixSetting(getenv, EnvCallDenyPrefixes)
	collect(err)

	p.MaxConcurrentCalls, err = intSetting(getenv, EnvMaxConcurrentCalls, p.MaxConcurrentCalls, MinMaxConcurrentCalls, MaxMaxConcurrentCalls)
	collect(err)
	p.UserMaxConcurrentCalls, err = intSetting(getenv, EnvUserMaxConcurrentCalls, p.UserMaxConcurrentCalls, MinMaxConcurrentCalls, MaxMaxConcurrentCalls)
	collect(err)
	p.UserMaxCallsPerHour, err = intSetting(getenv, EnvUserMaxCallsPerHour, p.UserMaxCallsPerHour, MinUserMaxCallsPerHour, MaxUserMaxCallsPerHour)
	collect(err)
	p.MaxCallDurationSeconds, err = intSetting(getenv, EnvMaxCallDurationSeconds, p.MaxCallDurationSeconds, MinMaxCallDurationSeconds, MaxMaxCallDurationSeconds)
	collect(err)
	p.RingTimeoutSeconds, err = intSetting(getenv, EnvRingTimeoutSeconds, p.RingTimeoutSeconds, MinRingTimeoutSeconds, MaxRingTimeoutSeconds)
	collect(err)

	p.MaxCallCostMicros, err = moneySetting(getenv, EnvMaxCallCost)
	collect(err)
	p.CallCostEstimateMicrosPerMinute, err = moneySetting(getenv, EnvCallCostEstimatePerMinute)
	collect(err)

	p.CallCostCurrency = strings.TrimSpace(getenv(EnvCallCostCurrency))
	costSet := p.MaxCallCostMicros != nil || p.CallCostEstimateMicrosPerMinute != nil
	switch {
	case p.CallCostCurrency != "" && !currencyPattern.MatchString(p.CallCostCurrency):
		collect(fmt.Errorf("%s: must be an ISO 4217 code of three uppercase letters", EnvCallCostCurrency))
	case costSet && p.CallCostCurrency == "":
		collect(fmt.Errorf("%s: required when %s or %s is set", EnvCallCostCurrency, EnvMaxCallCost, EnvCallCostEstimatePerMinute))
	}
	return p, errs
}

// prefixSetting parses a comma-separated E.164 prefix list. An unset variable
// is an empty list. An empty entry is an error, so a stray comma cannot pass.
func prefixSetting(getenv func(string) string, key string) ([]string, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return nil, nil
	}
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		p := strings.TrimSpace(entry)
		if !e164PrefixPattern.MatchString(p) {
			return nil, fmt.Errorf("%s: %q must be an E.164 prefix such as +1415", key, p)
		}
		out = append(out, p)
	}
	return out, nil
}

// intSetting parses a whole number within [lo, hi], or returns fallback when unset.
func intSetting(getenv func(string) string, key string, fallback, lo, hi int) (int, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: must be a whole number", key)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%s: must be between %d and %d", key, lo, hi)
	}
	return n, nil
}

// moneySetting parses an optional positive decimal amount into micro-units. It
// returns nil when the variable is unset.
func moneySetting(getenv func(string) string, key string) (*int64, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return nil, nil
	}
	v, err := parseMicros(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return &v, nil
}

// DefaultCallPolicy returns the call policy with every default applied and no
// cost settings. Tests and the app start from it, so defaults live in one place.
func DefaultCallPolicy() CallPolicy {
	return CallPolicy{
		MaxConcurrentCalls:     DefaultMaxConcurrentCalls,
		UserMaxConcurrentCalls: DefaultUserMaxConcurrentCalls,
		UserMaxCallsPerHour:    DefaultUserMaxCallsPerHour,
		MaxCallDurationSeconds: DefaultMaxCallDurationSeconds,
		RingTimeoutSeconds:     DefaultRingTimeoutSeconds,
	}
}

// ParseMicros converts a decimal amount such as "1.50" to micro-units exactly.
// Amounts must be greater than zero and have at most six decimal places.
func ParseMicros(raw string) (int64, error) {
	return parseMicros(raw)
}

// FormatMicros renders micro-units as a plain decimal, such as "1.5" or "2".
func FormatMicros(v int64) string {
	whole, frac := v/MicrosPerUnit, v%MicrosPerUnit
	if frac == 0 {
		return strconv.FormatInt(whole, 10)
	}
	return strings.TrimRight(fmt.Sprintf("%d.%06d", whole, frac), "0")
}

// ParseMicrosAllowZero converts a provider decimal amount such as "0.0106" to
// micro-units exactly. Unlike ParseMicros it accepts zero, because a provider
// can report a zero cost. It shares the same format: at most six decimal places
// and no sign or exponent.
func ParseMicrosAllowZero(raw string) (int64, error) {
	m := decimalMoney.FindStringSubmatch(raw)
	if m == nil {
		return 0, errors.New("must be a decimal amount such as 1.50 with at most 6 decimal places")
	}
	whole, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, errors.New("must be a decimal amount such as 1.50")
	}
	var frac int64
	if m[2] != "" {
		frac, err = strconv.ParseInt(m[2]+strings.Repeat("0", 6-len(m[2])), 10, 64)
		if err != nil {
			return 0, errors.New("must be a decimal amount such as 1.50")
		}
	}
	return whole*MicrosPerUnit + frac, nil
}

// parseMicros converts a decimal string such as "1.50" to micro-units exactly.
func parseMicros(raw string) (int64, error) {
	m := decimalMoney.FindStringSubmatch(raw)
	if m == nil {
		return 0, errors.New("must be a decimal amount such as 1.50 with at most 6 decimal places")
	}
	whole, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, errors.New("must be a decimal amount such as 1.50")
	}
	var frac int64
	if m[2] != "" {
		frac, err = strconv.ParseInt(m[2]+strings.Repeat("0", 6-len(m[2])), 10, 64)
		if err != nil {
			return 0, errors.New("must be a decimal amount such as 1.50")
		}
	}
	v := whole*MicrosPerUnit + frac
	if v <= 0 {
		return 0, errors.New("must be greater than zero")
	}
	return v, nil
}
