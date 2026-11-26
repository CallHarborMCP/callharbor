package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// BriefVersion is the schema version of the stored call brief. A later change
// to the brief shape must bump it, so old briefs stay readable as written.
const BriefVersion = 1

// Brief limits. Counts and per-item sizes are fixed here. The total byte limit
// is configuration (CALLHARBOR_MAX_CALL_BRIEF_BYTES) and is passed in.
const (
	maxObjectiveRunes      = 2000
	maxRecipientRunes      = 200
	maxLanguageLength      = 35
	maxListItems           = 20
	maxListItemRunes       = 500
	maxContextRunes        = 20000
	maxInstructionsRunes   = 8192
	defaultMaxDurationSecs = 600
	minMaxDurationSecs     = 1
	maxMaxDurationSecs     = 7200
)

// e164Pattern is the same expression as the to field in api/openapi.json.
var e164Pattern = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// languagePattern accepts BCP 47-shaped tags such as en, en-US, pt-BR and
// zh-Hant-TW. It is a shape check, not a registry lookup.
var languagePattern = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8})*$`)

// Brief is the canonical call brief. Its JSON encoding is stored verbatim and
// hashed, so the record of what was requested never changes.
type Brief struct {
	Version         int      `json:"version"`
	Recipient       string   `json:"recipient,omitempty"`
	Language        string   `json:"language,omitempty"`
	Objective       string   `json:"objective"`
	Context         string   `json:"context,omitempty"`
	Instructions    string   `json:"instructions,omitempty"`
	Constraints     []string `json:"constraints,omitempty"`
	Fallbacks       []string `json:"fallbacks,omitempty"`
	SuccessCriteria []string `json:"success_criteria,omitempty"`
}

// RenderedBrief is a validated brief with its canonical bytes and digest.
type RenderedBrief struct {
	Brief          Brief
	CanonicalJSON  string
	SHA256         string
	MaxDuration    int
	Record         bool
	NormalizedTo   string
	NormalizedLang string
}

// ValidateMakeCall checks every field of a make_call request. It returns the
// brief and the normalized request. maxBriefBytes is the configured limit on
// the canonical brief size. Oversized input is rejected, never truncated.
func ValidateMakeCall(in MakeCallInput, maxBriefBytes int) (RenderedBrief, error) {
	if !validFieldID(in.ProfileID) {
		return RenderedBrief{}, invalidInput("profile_id must be 1 to %d characters", MaxIDLength)
	}
	if !e164Pattern.MatchString(in.To) {
		return RenderedBrief{}, invalidInput("to must be an E.164 number such as +14155550100")
	}
	if strings.TrimSpace(in.Objective) == "" {
		return RenderedBrief{}, invalidInput("objective is required")
	}
	if err := checkRunes("objective", in.Objective, maxObjectiveRunes); err != nil {
		return RenderedBrief{}, err
	}
	if err := checkRunes("recipient", in.Recipient, maxRecipientRunes); err != nil {
		return RenderedBrief{}, err
	}
	if in.Language != "" {
		if len(in.Language) > maxLanguageLength || !languagePattern.MatchString(in.Language) {
			return RenderedBrief{}, invalidInput("language must be a BCP 47 tag of at most %d characters", maxLanguageLength)
		}
	}
	if err := checkRunes("context", in.Context, maxContextRunes); err != nil {
		return RenderedBrief{}, err
	}
	if err := checkRunes("instructions", in.Instructions, maxInstructionsRunes); err != nil {
		return RenderedBrief{}, err
	}
	for name, list := range map[string][]string{
		"constraints": in.Constraints, "fallbacks": in.Fallbacks, "success_criteria": in.SuccessCriteria,
	} {
		if len(list) > maxListItems {
			return RenderedBrief{}, invalidInput("%s may hold at most %d items", name, maxListItems)
		}
		for i, item := range list {
			if strings.TrimSpace(item) == "" {
				return RenderedBrief{}, invalidInput("%s[%d] must not be empty", name, i)
			}
			if err := checkRunes(fmt.Sprintf("%s[%d]", name, i), item, maxListItemRunes); err != nil {
				return RenderedBrief{}, err
			}
		}
	}
	maxDur := defaultMaxDurationSecs
	if in.MaxDuration != nil {
		if *in.MaxDuration < minMaxDurationSecs || *in.MaxDuration > maxMaxDurationSecs {
			return RenderedBrief{}, invalidInput("max_duration_seconds must be between %d and %d", minMaxDurationSecs, maxMaxDurationSecs)
		}
		maxDur = *in.MaxDuration
	}

	brief := Brief{
		Version:         BriefVersion,
		Recipient:       in.Recipient,
		Language:        in.Language,
		Objective:       in.Objective,
		Context:         in.Context,
		Instructions:    in.Instructions,
		Constraints:     nonEmpty(in.Constraints),
		Fallbacks:       nonEmpty(in.Fallbacks),
		SuccessCriteria: nonEmpty(in.SuccessCriteria),
	}
	canonical, err := json.Marshal(brief)
	if err != nil {
		return RenderedBrief{}, internal(fmt.Errorf("marshal brief: %w", err))
	}
	if len(canonical) > maxBriefBytes {
		return RenderedBrief{}, newError(CodeInvalidInput, fmt.Sprintf(
			"brief_too_large: the call brief is %d bytes; the limit is %d bytes (CALLHARBOR_MAX_CALL_BRIEF_BYTES)",
			len(canonical), maxBriefBytes))
	}
	sum := sha256.Sum256(canonical)
	return RenderedBrief{
		Brief:          brief,
		CanonicalJSON:  string(canonical),
		SHA256:         hex.EncodeToString(sum[:]),
		MaxDuration:    maxDur,
		Record:         in.Record,
		NormalizedTo:   in.To,
		NormalizedLang: in.Language,
	}, nil
}

// checkRunes rejects a string longer than max runes. Empty strings pass.
func checkRunes(name, s string, max int) error {
	if !utf8.ValidString(s) {
		return invalidInput("%s must be valid UTF-8", name)
	}
	if utf8.RuneCountInString(s) > max {
		return invalidInput("%s may be at most %d characters", name, max)
	}
	return nil
}

// nonEmpty returns nil for an empty list so omitempty drops it from the brief.
func nonEmpty(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	return list
}

// validFieldID checks an identifier from the request. It is non-empty and
// within the shared length limit.
func validFieldID(id string) bool {
	return id != "" && len(id) <= MaxIDLength
}
