// Package callpolicy resolves the effective outbound call policy for one owner.
// The installation policy (environment) and the owner's administrator-set
// overrides are combined here, so MakeCall and the dial worker apply the same
// rules. Every numeric limit can only tighten the installation value, and a
// destination must pass every list that applies.
package callpolicy

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// Reason codes. They are part of the public contract and appear as the
// machine prefix of the error message or the call's failure reason.
const (
	ReasonDestinationDenied      = "destination_denied"
	ReasonDestinationNotAllowed  = "destination_not_allowed"
	ReasonDurationExceedsPolicy  = "max_duration_exceeds_policy"
	ReasonCostUnavailable        = "cost_unavailable"
	ReasonCostLimitExceeded      = "cost_limit_exceeded"
	ReasonConcurrencyLimit       = "concurrency_limit"
	ReasonGlobalConcurrencyLimit = "global_concurrency_limit"
	ReasonRateLimited            = "rate_limited"
)

// SchemaMaxDurationSeconds is the largest call duration the schema allows. It
// is the effective limit when the installation sets none.
const SchemaMaxDurationSeconds = 7200

var prefixPattern = regexp.MustCompile(`^\+[1-9][0-9]{0,14}$`)

// Effective is the policy that applies to one owner right now. A zero count
// means no limit. config.Load never produces zero limits, but the zero value
// of Effective is safe to read.
type Effective struct {
	// GlobalMaxConcurrent limits non-terminal calls across the installation.
	GlobalMaxConcurrent int
	// MaxConcurrentCalls limits non-terminal calls for this owner.
	MaxConcurrentCalls int
	// MaxCallsPerHour limits calls created by this owner in the trailing hour.
	MaxCallsPerHour int
	// MaxDurationSeconds is the longest call this owner may place.
	MaxDurationSeconds int
	// MaxCostMicros is the per-call cost limit in micro-units, nil when none.
	MaxCostMicros *int64
	// CostCurrency is the ISO 4217 code for every cost figure.
	CostCurrency string
	// EstimateMicrosPerMinute is the operator-supplied estimate rate, nil when
	// none is configured. It is never a provider charge.
	EstimateMicrosPerMinute *int64
	// DenyPrefixes is the union of the installation and owner deny lists.
	DenyPrefixes []string
	// AllowGroups holds each non-empty allow list. A destination must match a
	// prefix in every group.
	AllowGroups [][]string
}

// Resolve combines the installation policy with the owner's overrides. Limits
// take the stricter value. A nil override inherits the installation value.
func Resolve(install config.CallPolicy, user storage.UserCallPolicy) Effective {
	e := Effective{
		GlobalMaxConcurrent: install.MaxConcurrentCalls,
		MaxConcurrentCalls:  minLimit(install.UserMaxConcurrentCalls, user.MaxConcurrentCalls),
		MaxCallsPerHour:     minLimit(install.UserMaxCallsPerHour, user.MaxCallsPerHour),
		MaxDurationSeconds:  minLimit(installDuration(install), user.MaxDurationSeconds),
		CostCurrency:        install.CallCostCurrency,
	}
	e.MaxCostMicros = minCost(install.MaxCallCostMicros, user.MaxCostMicros)
	e.EstimateMicrosPerMinute = install.CallCostEstimateMicrosPerMinute

	e.DenyPrefixes = append(append([]string{}, install.DenyPrefixes...), user.DenyPrefixes...)
	if len(install.AllowPrefixes) > 0 {
		e.AllowGroups = append(e.AllowGroups, append([]string{}, install.AllowPrefixes...))
	}
	if len(user.AllowPrefixes) > 0 {
		e.AllowGroups = append(e.AllowGroups, append([]string{}, user.AllowPrefixes...))
	}
	return e
}

// installDuration is the installation maximum call duration, or the schema
// maximum when the installation policy does not set one.
func installDuration(install config.CallPolicy) int {
	if install.MaxCallDurationSeconds <= 0 {
		return SchemaMaxDurationSeconds
	}
	return install.MaxCallDurationSeconds
}

// CheckDestination returns "" when to may be dialed, or the reason code that
// refuses it. Deny wins over allow, and every non-empty allow group must match.
func CheckDestination(e Effective, to string) string {
	for _, p := range e.DenyPrefixes {
		if strings.HasPrefix(to, p) {
			return ReasonDestinationDenied
		}
	}
	for _, group := range e.AllowGroups {
		if !matchesAny(to, group) {
			return ReasonDestinationNotAllowed
		}
	}
	return ""
}

func matchesAny(to string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(to, p) {
			return true
		}
	}
	return false
}

// EstimateMicros returns rate × ceil(durationSeconds / 60) in micro-units. ok is
// false when the product would overflow int64, which callers treat as a refusal.
func EstimateMicros(ratePerMinute int64, durationSeconds int) (int64, bool) {
	minutes := int64((durationSeconds + 59) / 60)
	if minutes <= 0 {
		return 0, true
	}
	if ratePerMinute > math.MaxInt64/minutes {
		return 0, false
	}
	return ratePerMinute * minutes, true
}

// ValidatePrefixes checks that every entry is an E.164 prefix such as +1415.
// The error names the first bad entry, never the whole list.
func ValidatePrefixes(prefixes []string) error {
	for _, p := range prefixes {
		if !prefixPattern.MatchString(p) {
			return fmt.Errorf("%q must be an E.164 prefix such as +1415", p)
		}
	}
	return nil
}

// minLimit returns the stricter of an installation limit and an optional
// override. Zero means no limit, so an unset override keeps the installation
// value and a zero installation limit lets the override stand.
func minLimit(install int, override *int64) int {
	if override == nil {
		return install
	}
	return int(minPositive(int64(install), *override))
}

func minPositive(a, b int64) int64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// minCost returns the stricter cost limit. nil means no limit.
func minCost(install, override *int64) *int64 {
	switch {
	case install == nil:
		return override
	case override == nil:
		return install
	case *install < *override:
		return install
	default:
		return override
	}
}
