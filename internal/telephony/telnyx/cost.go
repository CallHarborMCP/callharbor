package telnyx

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// EventCallCost is the Telnyx call.cost webhook. It reports the provider's
// charge for a call. It is not a call state transition.
const EventCallCost = "call.cost"

// Cost status values Telnyx reports on call.cost.
const (
	CostStatusSuccess = "success"
	CostStatusError   = "error"
)

// CostPart is one line of the provider's cost breakdown. Amounts are decimal
// strings exactly as Telnyx sends them.
type CostPart struct {
	Cost     string
	Currency string
}

// CostReport is the cost half of a call.cost webhook. Status is success or
// error. TotalCost is the provider's total as a decimal string, empty when
// absent. Parts carry each line's currency, so the caller can refuse a mix.
type CostReport struct {
	Status    string
	TotalCost string
	Parts     []CostPart
}

// decimalAmount is a non-negative decimal string with at most six decimal
// places, as Telnyx sends amounts. Exponents and signs are refused.
var decimalAmount = regexp.MustCompile(`^[0-9]{1,12}(?:\.[0-9]{1,6})?$`)

// costPayload is the call.cost payload fields CallHarbor reads.
type costPayload struct {
	Status    string  `json:"status"`
	TotalCost *string `json:"total_cost"`
	CostParts []struct {
		Cost     string `json:"cost"`
		Currency string `json:"currency"`
	} `json:"cost_parts"`
}

// parseCost reads the cost report from a call.cost payload. It refuses a status
// other than success or error, and an amount that is not a plain decimal. Those
// are input errors, so the event is refused rather than stored as a guess.
func parseCost(raw json.RawMessage) (CostReport, error) {
	var p costPayload
	if len(raw) == 0 || json.Unmarshal(raw, &p) != nil {
		return CostReport{}, fmt.Errorf("%w: cost payload", ErrInvalidWebhook)
	}
	if p.Status != CostStatusSuccess && p.Status != CostStatusError {
		return CostReport{}, fmt.Errorf("%w: cost status", ErrInvalidWebhook)
	}
	rep := CostReport{Status: p.Status}
	if p.TotalCost != nil {
		total := strings.TrimSpace(*p.TotalCost)
		if total != "" && !decimalAmount.MatchString(total) {
			return CostReport{}, fmt.Errorf("%w: total_cost", ErrInvalidWebhook)
		}
		rep.TotalCost = total
	}
	for _, part := range p.CostParts {
		if part.Cost != "" && !decimalAmount.MatchString(part.Cost) {
			return CostReport{}, fmt.Errorf("%w: cost part amount", ErrInvalidWebhook)
		}
		rep.Parts = append(rep.Parts, CostPart{Cost: part.Cost, Currency: part.Currency})
	}
	return rep, nil
}
