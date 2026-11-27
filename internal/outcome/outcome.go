// Package outcome evaluates the task outcome of one call from its technical
// facts, its transcript and the voice session's self-report. It is pure: it
// reads no storage, clock or network, so the same input always gives the same
// result. A successful dial or a normal hangup is never task success by itself.
package outcome

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/CallHarborMCP/callharbor/internal/call"
)

// Outcome is the task result, independent of the technical call status.
type Outcome string

const (
	Success Outcome = "success"
	Partial Outcome = "partial"
	Failed  Outcome = "failed"
	Unknown Outcome = "unknown"
)

// Source names what produced a result.
type Source string

const (
	// SourceTechnical means no voice-session assessment was used.
	SourceTechnical Source = "technical"
	// SourceVoiceSession means the result rests on a voice-session assessment.
	SourceVoiceSession Source = "voice_session"
)

// Protocols a call can run on.
const (
	ProtocolRealtimeWS = "realtime_ws"
	ProtocolLiveWS     = "live_ws"
)

// Evidence kinds.
const (
	KindTranscriptTurn = "transcript_turn"
	KindCallEvent      = "call_event"
)

// Limits.
const (
	// MaxEvidence bounds the evidence items in one result.
	MaxEvidence = 32
	// MaxQuoteRunes bounds one quote. A longer quote is unverified.
	MaxQuoteRunes = 500
)

// Model reasons. The set is closed; an assessment with any other reason is
// not valid and is ignored.
const (
	ReasonConfirmed           = "confirmed"
	ReasonAlternativeAccepted = "alternative_accepted"
	ReasonPartiallyFulfilled  = "partially_fulfilled"
	ReasonCalleeRefused       = "callee_refused"
	ReasonCalleeUnable        = "callee_unable"
	ReasonVoicemail           = "voicemail"
	ReasonWrongParty          = "wrong_party"
	ReasonOther               = "other"
)

// Reason codes the evaluator emits.
const (
	CodeNoAnswer                = "no_answer"
	CodeBusy                    = "busy"
	CodeRejected                = "rejected"
	CodeCallFailed              = "call_failed"
	CodeAssessmentMissing       = "assessment_missing"
	CodeAssessmentInvalid       = "assessment_invalid"
	CodeAssessmentUnsupported   = "assessment_unsupported"
	CodeAssessmentContradictory = "assessment_contradictory"
	// CodeAssessmentConflicting means valid reports disagree on the outcome. The
	// result is unknown; the last report does not win.
	CodeAssessmentConflicting = "assessment_conflicting"
	CodeEvidenceUnverified    = "evidence_unverified"
	CodeEvidenceMissing       = "evidence_missing"
	CodeEvidenceTruncated     = "evidence_truncated"
	CodeCriteriaUnmet         = "criteria_unmet"
	CodeConstraintViolated    = "constraint_violated"
	// CodeConstraintUnreported means a success omitted a brief constraint. An
	// omitted constraint is not treated as respected.
	CodeConstraintUnreported  = "constraint_unreported"
	CodeMediaFailed           = "media_failed"
	CodeTranscriptUnavailable = "transcript_unavailable"
	CodeTranscriptIncomplete  = "transcript_incomplete"
	CodeNoConversation        = "no_conversation"
	// CodeEvaluationBoundExceeded means the call is larger than the evaluation
	// bounds, so it is recorded unknown instead of being retried forever.
	CodeEvaluationBoundExceeded = "evaluation_bound_exceeded"
	// CodeEvaluationInputInvalid means the stored brief or facts cannot be read,
	// so the call is recorded unknown instead of being retried forever.
	CodeEvaluationInputInvalid = "evaluation_input_invalid"
)

// Input is everything one evaluation reads. The caller assembles it from
// storage; the evaluator performs no I/O.
type Input struct {
	// Protocol is the voice protocol the call ran on.
	Protocol string
	// Answered reports whether call.answered was recorded for the call.
	Answered bool
	// ReasonCode is the call's technical reason, calls.outcome_reason_code.
	ReasonCode string
	// MediaFailed reports whether the media session failed during the call.
	MediaFailed bool
	// CriteriaCount is the number of success criteria in the immutable brief.
	CriteriaCount int
	// ConstraintCount is the number of constraints in the immutable brief. A
	// success needs every one of them reported as respected.
	ConstraintCount int
	Transcript      Transcript
	// CallEvents are the technical events cited as evidence, in order.
	CallEvents []CallEventRef
	// Assessments are the voice-session self-reports in arrival order.
	Assessments []Assessment
	// Prepared is optional. A caller that also resolves report quotes sets it to
	// Prepare(Transcript.Turns), so each turn is tokenized once per evaluation.
	// When nil, Evaluate prepares the transcript itself.
	Prepared *Prepared
	// InvalidAssessments counts stored self-reports that failed validation.
	// They are skipped, and the result carries assessment_invalid.
	InvalidAssessments int
}

// Transcript is the call's transcript state and turns.
type Transcript struct {
	Started   bool
	Finalized bool
	Turns     []Turn
}

// Turn is one transcript turn. Only final callee turns can support evidence.
type Turn struct {
	Seq      int
	Speaker  string
	Finality string
	Text     string
	Fidelity string
}

// CallEventRef is a technical call event cited as evidence.
type CallEventRef struct {
	Seq      int
	Code     string
	Fidelity string
}

// Assessment is one voice-session self-report, as claimed. Claims are checked
// against the transcript before any of them count.
type Assessment struct {
	Outcome string
	Reason  string
	// ConstraintsViolated is true when any constraint was reported not respected.
	ConstraintsViolated bool
	// ConstraintsReported lists the 1-based constraint indexes the session
	// reported on. A success needs every constraint present here.
	ConstraintsReported []int
	Claims              []Claim
}

// Claim ties a quote to a transcript turn and optionally to a criterion. Met
// says whether the session reported the criterion as met. An unmet claim keeps
// its quote, so a refusal can be shown, but only met criteria count toward
// success or partial.
type Claim struct {
	// Criterion is 1-based into the brief's criteria. 0 means the quote
	// supports no particular criterion (outcome-level evidence).
	Criterion int
	Seq       int
	Quote     string
	Met       bool
}

// Evidence is one reference that supports a result. Met is set only on
// transcript evidence that backs a criterion; it is omitted for call events and
// outcome-level quotes.
//
// Evidence is a reference, not content. It never carries the callee's words: a
// reader gets the text from the audited transcript endpoint at Seq. The quote a
// report cites is used only to verify the claim and is not stored.
type Evidence struct {
	Kind      string `json:"kind"`
	Seq       int    `json:"seq"`
	Criterion *int   `json:"criterion,omitempty"`
	Met       *bool  `json:"met,omitempty"`
	Speaker   string `json:"speaker,omitempty"`
	Fidelity  string `json:"fidelity,omitempty"`
	Code      string `json:"code,omitempty"`
}

// Result is the evaluated task outcome.
type Result struct {
	Outcome     Outcome    `json:"outcome"`
	ReasonCodes []string   `json:"reason_codes"`
	Evidence    []Evidence `json:"evidence"`
	Source      Source     `json:"source"`
}

// Unrecordable is the unknown result for a call whose evaluation input cannot be
// used: the transcript is over the evaluation bound, or its brief or facts are
// unreadable. It is technical, so it names only the code and carries no evidence.
// It is recorded so such a call is not retried forever. code must be one of the
// evaluation_* codes.
func Unrecordable(code string) Result {
	return finish(Unknown, SourceTechnical, []string{code}, nil, nil)
}

// Evaluate computes the task outcome. A call that was never answered is failed
// with a technical code. An answered call is unknown unless a valid assessment
// is verified against the transcript.
func Evaluate(in Input) Result {
	if !in.Answered {
		codes := appendCode(nil, notAnsweredCode(in.ReasonCode))
		return finish(Failed, SourceTechnical, codes, callEventEvidence(in.CallEvents), nil)
	}

	codes := transcriptCodes(nil, in.Transcript)
	if in.MediaFailed {
		codes = appendCode(codes, CodeMediaFailed)
	}
	callEv := callEventEvidence(in.CallEvents)

	a, ok := lastValid(in.Assessments)
	if !ok {
		// Invalid reports replace "missing": the session did report, badly.
		code := CodeAssessmentMissing
		switch {
		case in.Protocol == ProtocolLiveWS:
			code = CodeAssessmentUnsupported
		case in.InvalidAssessments > 0:
			code = CodeAssessmentInvalid
		}
		return finish(Unknown, SourceTechnical, appendCode(codes, code), callEv, nil)
	}
	if in.InvalidAssessments > 0 {
		codes = appendCode(codes, CodeAssessmentInvalid)
	}
	// Valid reports that disagree on the outcome cannot be resolved by taking
	// the last one. The result is unknown and no report's claims are used.
	if conflicting(in.Assessments) {
		return finish(Unknown, SourceTechnical, appendCode(codes, CodeAssessmentConflicting), callEv, nil)
	}
	codes = appendCode(codes, a.Reason)

	verified, unverified := verifyClaims(in, a, in.prepared())
	if unverified {
		codes = appendCode(codes, CodeEvidenceUnverified)
	}
	o, codes := decide(in, a, verified, codes)
	return finish(o, SourceVoiceSession, codes, callEv, verified)
}

// decide applies the claimed outcome to what the transcript verifies. A claim
// is downgraded, never upgraded, when its requirements are not met.
func decide(in Input, a Assessment, verified []Evidence, codes []string) (Outcome, []string) {
	// met holds only criteria the session reported met AND the transcript
	// verifies. A quote for an unmet criterion is evidence, never a success.
	met := map[int]bool{}
	for _, e := range verified {
		if e.Criterion != nil && e.Met != nil && *e.Met {
			met[*e.Criterion] = true
		}
	}
	switch Outcome(a.Outcome) {
	case Success:
		return decideSuccess(in, a, verified, met, codes)
	case Partial:
		if successReason(a.Reason) {
			return Unknown, appendCode(codes, CodeAssessmentContradictory)
		}
		if len(met) > 0 {
			return Partial, codes
		}
		return Unknown, appendCode(codes, CodeEvidenceMissing)
	case Failed:
		if successReason(a.Reason) {
			return Unknown, appendCode(codes, CodeAssessmentContradictory)
		}
		// Any verified quote supports a failure, including a quote for a criterion
		// the session reported unmet, such as a refusal.
		if len(verified) > 0 {
			return Failed, codes
		}
		return Unknown, appendCode(codes, CodeEvidenceMissing)
	default:
		return Unknown, codes
	}
}

// decideSuccess requires a success reason, every brief constraint reported and
// respected, no media failure, a finalized transcript, and either every
// criterion met and verified or, with no criteria, one verified outcome-level
// quote.
func decideSuccess(in Input, a Assessment, verified []Evidence, met map[int]bool, codes []string) (Outcome, []string) {
	if !successReason(a.Reason) {
		return Unknown, appendCode(codes, CodeAssessmentContradictory)
	}
	if a.ConstraintsViolated {
		return Unknown, appendCode(codes, CodeConstraintViolated)
	}
	if !constraintsReported(in.ConstraintCount, a.ConstraintsReported) {
		// An omitted constraint is not treated as respected.
		return Unknown, appendCode(codes, CodeConstraintUnreported)
	}
	if in.MediaFailed || !in.Transcript.Finalized {
		// The reason code for either is already recorded.
		return Unknown, codes
	}
	if in.CriteriaCount == 0 {
		if hasMetEvidence(verified) {
			return Success, codes
		}
		return Unknown, appendCode(codes, CodeEvidenceMissing)
	}
	if allCriteriaMet(in.CriteriaCount, met) {
		return Success, codes
	}
	if len(met) > 0 {
		return Partial, appendCode(codes, CodeCriteriaUnmet)
	}
	return Unknown, appendCode(codes, CodeEvidenceMissing)
}

// constraintsReported reports whether every brief constraint 1..n appears in
// the session's report.
func constraintsReported(n int, reported []int) bool {
	for i := 1; i <= n; i++ {
		if !slices.Contains(reported, i) {
			return false
		}
	}
	return true
}

// hasMetEvidence reports whether any verified evidence is met or outcome-level.
func hasMetEvidence(verified []Evidence) bool {
	for _, e := range verified {
		if e.Met == nil || *e.Met {
			return true
		}
	}
	return false
}

// conflicting reports whether two valid assessments name different outcomes.
func conflicting(as []Assessment) bool {
	var first string
	for _, a := range as {
		if !validOutcome(a.Outcome) || !validReason(a.Reason) {
			continue
		}
		if first == "" {
			first = a.Outcome
		} else if a.Outcome != first {
			return true
		}
	}
	return false
}

func allCriteriaMet(n int, met map[int]bool) bool {
	for i := 1; i <= n; i++ {
		if !met[i] {
			return false
		}
	}
	return true
}

// evidenceKey identifies one reference. Evidence carries no quote, so two claims
// that cite the same turn for the same criterion and met value are one reference.
// preparedTurn is a final callee turn with its text tokenized once.
type preparedTurn struct {
	turn   Turn
	tokens []token
}

// Prepared is a transcript's final callee turns, each tokenized once. Building
// it once per evaluation keeps the cost to one tokenization per turn, however
// many claims or report quotes are checked against it.
type Prepared struct {
	bySeq map[int]preparedTurn
	// callee is the same turns in transcript order, for quote resolution.
	callee []preparedTurn
}

// Prepare tokenizes the final callee turns of a transcript. A repeated seq keeps
// its first turn, as verification always did.
func Prepare(turns []Turn) *Prepared {
	p := &Prepared{bySeq: map[int]preparedTurn{}}
	for _, t := range turns {
		if t.Speaker != "callee" || t.Finality != "final" {
			continue
		}
		pt := preparedTurn{turn: t, tokens: tokenize(t.Text)}
		p.callee = append(p.callee, pt)
		if _, dup := p.bySeq[t.Seq]; !dup {
			p.bySeq[t.Seq] = pt
		}
	}
	return p
}

// turn returns the prepared callee turn with seq, and whether there is one.
func (p *Prepared) turn(seq int) (preparedTurn, bool) {
	pt, ok := p.bySeq[seq]
	return pt, ok
}

// prepared returns the Input's prepared transcript, building it when the caller
// did not supply one.
func (in Input) prepared() *Prepared {
	if in.Prepared != nil {
		return in.Prepared
	}
	return Prepare(in.Transcript.Turns)
}

type evidenceKey struct {
	seq       int
	criterion int
	met       bool
}

// verifyClaims returns the evidence for every claim whose quote is supported by
// a final callee turn with the claimed seq (see supportsTurn). Other claims are
// dropped, and the bool reports whether any were dropped.
//
// Matching is whole-word, not a normalized substring, so a quote such as "no"
// cannot match inside "not". This is a documented design choice (see
// docs/operations/api.md, Evidence semantics).
func verifyClaims(in Input, a Assessment, p *Prepared) ([]Evidence, bool) {
	var out []Evidence
	seen := map[evidenceKey]bool{}
	unverified := false
	for _, c := range a.Claims {
		if !c.Met && len(tokenize(c.Quote)) == 0 {
			// An unmet criterion with no quote makes no claim to verify, so it
			// adds no evidence_unverified.
			continue
		}
		ev, ok := verifyClaim(c, in.CriteriaCount, negationChecked(c, a.Outcome), p)
		if !ok {
			unverified = true
			continue
		}
		key := evidenceKey{seq: ev.Seq, criterion: criterionOf(ev), met: ev.Met != nil && *ev.Met}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ev)
	}
	return out, unverified
}

// Minimum quote strength. A shorter quote such as "yes" proves nothing on its
// own, so it is unverified.
const (
	minQuoteWords   = 3
	minQuoteLetters = 12
)

// negations are tokens that flip the meaning of the words after them. A met
// quote that matches later in the same clause is unverified. "no" is handled
// separately by negatedAt, so "No problem, booked a table" is not negated.
//
// This list is a heuristic, not a parser. "whether" and "unsure" are judgment
// calls: an uncertain statement is not a confirmation, so they fail safe.
var negations = map[string]bool{
	"not": true, "never": true, "cannot": true, "cant": true,
	"wont": true, "dont": true, "isnt": true, "unable": true,
	"nobody": true, "nothing": true, "neither": true, "nor": true, "without": true,
	"unsure": true, "whether": true,
}

// clausePunct are the runes that end a clause for the "no" rule. Dashes are not
// in this set; they end a negation's clause through the clause flag instead
// (see tokenize and negatedAt).
const clausePunct = ",;:.!?"

// abbreviations are words whose following full stop does not end a sentence,
// such as "Dr. Smith". Only a full stop is affected, never "!" or "?".
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "st": true,
	"jr": true, "sr": true, "vs": true,
}

// apostrophes are the runes that join a contraction, such as can't. The ASCII
// apostrophe, the typographic quotes, the modifier letter apostrophe, the
// fullwidth apostrophe, the prime and the backtick all join words the same way.
const apostrophes = "'\u2019\u2018\u02bc\uff07\u2032`"

// token is one word of a turn or a quote: lowercased, letters and digits only.
//   - brk reports whether clause punctuation sits between this word and the one
//     before it.
//   - clause reports whether a clause break sits between them: clause
//     punctuation other than an abbreviation's full stop, or a dash. A hyphen
//     inside a word ("re-book") is not a break. A negation's scope ends here.
//   - sent reports whether sentence-ending punctuation sits between them.
//   - contraction reports whether the word was joined across an apostrophe and
//     ends in "nt", such as "didn't" or "haven't". Such a word is a negation.
//
// An apostrophe joins its letters into one word, so "can't" is "cant".
type token struct {
	word        string
	brk         bool
	clause      bool
	sent        bool
	contraction bool
}

// negating reports whether the token is a negation by itself: a negation word
// or a contraction ending in "nt".
func (t token) negating() bool {
	return negations[t.word] || t.contraction
}

// tokenize splits raw text into words. It works on the raw text, not on a
// normalized copy, so clause and sentence punctuation are still known when
// negation is checked.
func tokenize(s string) []token {
	var out []token
	var cur strings.Builder
	brk, clause, sent, joined, pending := false, false, false, false, false
	// afterApos holds the letters that follow the last apostrophe in the word.
	// It decides the n't rule: only the suffix "t" after an apostrophe, on a
	// word ending in "nt", is a contraction.
	var afterApos strings.Builder
	// lastWord is the word most recently flushed, so a full stop can be judged
	// by the word before it (for abbreviations).
	lastWord := ""
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		w := cur.String()
		out = append(out, token{
			word: w, brk: brk, clause: clause, sent: sent,
			contraction: joined && isNtContraction(w, afterApos.String()),
		})
		lastWord = w
		cur.Reset()
		afterApos.Reset()
		brk, clause, sent, joined, pending = false, false, false, false, false
	}
	rs := []rune(s)
	for i, r := range rs {
		r = unicode.ToLower(r)
		switch {
		case strings.ContainsRune(apostrophes, r):
			// Dropped, so the letters on either side join into one word. Whether
			// the word is joined is decided by the letter that follows: a closing
			// quote such as 'agent' is followed by a space, so it does not join.
			pending = cur.Len() > 0
			afterApos.Reset()
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if pending {
				joined = true
				pending = false
			}
			cur.WriteRune(r)
			afterApos.WriteRune(r)
		default:
			// A full stop between two digits ("8.30", "$12.50") is part of a
			// number, not a break at all.
			if r == '.' && i > 0 && i+1 < len(rs) && unicode.IsDigit(rs[i-1]) && unicode.IsDigit(rs[i+1]) {
				cur.WriteRune(r)
				continue
			}
			flush()
			if strings.ContainsRune(clausePunct, r) {
				brk = true
				// A full stop after an abbreviation or an initial ("Dr.", "p.m.")
				// does not end a clause, so "not Dr. Lee booked" keeps its negation.
				if r != '.' || !abbreviatedStop(lastWord) {
					clause = true
				}
			}
			// A dash ends a negation's clause, but a hyphen inside a word
			// ("re-book") does not.
			hyphenInWord := r == '-' && isWordRune(rs, i-1) && isWordRune(rs, i+1)
			if isDash(r) && !hyphenInWord {
				clause = true
			}
			if isSentenceEnd(rs, i, lastWord) {
				sent = true
			}
		}
	}
	flush()
	return out
}

// isSentenceEnd reports whether the rune at i ends a sentence. "!" and "?" always
// do. A full stop ends one only when it is followed by whitespace and then an
// uppercase letter, or by the end of the text, and the word before it is not a
// single letter, all digits, or an abbreviation such as "Dr". So "8 p.m. tonight"
// and "Dr. Smith" do not end a sentence, and "Lunch is free. Book" does.
func isSentenceEnd(rs []rune, i int, prevWord string) bool {
	switch rs[i] {
	case '!', '?':
		return true
	case '.':
	default:
		return false
	}
	// Judge the full stop by the word before it. A word that is one letter,
	// all digits, or an abbreviation does not end a sentence.
	if prevWord == "" || abbreviations[prevWord] || allDigits(prevWord) || len([]rune(prevWord)) == 1 {
		return false
	}
	j := i + 1
	for j < len(rs) && unicode.IsSpace(rs[j]) {
		j++
	}
	if j == len(rs) {
		return true
	}
	return j > i+1 && unicode.IsUpper(rs[j])
}

// isDash reports whether r is a hyphen-minus, en dash or em dash.
func isDash(r rune) bool {
	return r == '-' || r == '\u2013' || r == '\u2014'
}

// isWordRune reports whether rs[i] exists and is a letter or digit.
func isWordRune(rs []rune, i int) bool {
	return i >= 0 && i < len(rs) && (unicode.IsLetter(rs[i]) || unicode.IsDigit(rs[i]))
}

// abbreviatedStop reports whether a full stop after word w is part of an
// abbreviation or an initial, rather than a clause end.
func abbreviatedStop(w string) bool {
	rs := []rune(w)
	return abbreviations[w] || (len(rs) == 1 && unicode.IsLetter(rs[0]))
}

// allDigits reports whether w is made only of digits.
func allDigits(w string) bool {
	for _, r := range w {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return w != ""
}

// isNtContraction reports whether a word joined across an apostrophe is an
// n't contraction. The letters after the last apostrophe must be exactly "t",
// and the word must end in "nt". So "didn't" and "can't" qualify. "l'instant"
// and "o'grant" do not: their last apostrophe is followed by more than "t".
func isNtContraction(w, afterApos string) bool {
	return afterApos == "t" && strings.HasSuffix(w, "nt")
}

// verifyClaim checks one claim against the turn it cites. The evidence is
// returned only when the criterion is in range and the cited turn supports the
// quote (see supportsTurn).
func verifyClaim(c Claim, criteria int, negate bool, p *Prepared) (Evidence, bool) {
	if c.Criterion < 0 || c.Criterion > criteria {
		return Evidence{}, false
	}
	t, ok := p.turn(c.Seq)
	if !ok || !supportsTurn(c.Quote, negate, t) {
		return Evidence{}, false
	}
	ev := Evidence{Kind: KindTranscriptTurn, Seq: c.Seq, Speaker: t.turn.Speaker, Fidelity: t.turn.Fidelity}
	if c.Criterion > 0 {
		n := c.Criterion
		ev.Criterion = &n
		m := c.Met
		ev.Met = &m
	}
	return ev, true
}

// strongQuote reports whether a quote has enough words or letters to mean
// something by itself.
func strongQuote(qt []token) bool {
	if len(qt) == 0 {
		return false
	}
	letters := 0
	for _, t := range qt {
		for _, r := range t.word {
			if unicode.IsLetter(r) {
				letters++
			}
		}
	}
	return len(qt) >= minQuoteWords || letters >= minQuoteLetters
}

// negationChecked reports whether negation applies to a claim under outcome o.
// Only a met claim is checked, so an unmet quote such as a refusal is never
// read as negated. Outcome-level evidence (criterion 0) is not checked under a
// failed outcome, because any verified quote supports failure.
func negationChecked(c Claim, o string) bool {
	if !c.Met {
		return false
	}
	return Outcome(o) != Failed || c.Criterion != 0
}

// supportsTurn reports whether a quote is supported by turn t. The quote must be
// strong enough and must occur in t as whole words. When negate is set, an
// occurrence that is negated is not support, so "we can't book a table for two"
// does not support "book a table for two".
//
// The check is conservative and deterministic. It reads only the callee's own
// turn, and the quote and the turn are matched by the same tokenizer, so
// resolution and verification cannot disagree.
func supportsTurn(quote string, negate bool, t preparedTurn) bool {
	if utf8.RuneCountInString(quote) > MaxQuoteRunes {
		return false
	}
	qt := tokenize(quote)
	if !strongQuote(qt) {
		return false
	}
	tt := t.tokens
	if negate && quoteIsNegative(qt) {
		// A met quote that is itself a negative statement, such as "could not
		// book a table", is never support for a met criterion.
		return false
	}
	found := false
	for i := 0; i+len(qt) <= len(tt); i++ {
		if !sameWords(tt[i:i+len(qt)], qt) {
			continue
		}
		if negate && (negatedAt(tt, i) || spansSentence(tt, i, len(qt))) {
			return false
		}
		found = true
	}
	return found
}

// quoteIsNegative reports whether a met quote contains negation by itself: a
// negation token or contraction anywhere in it, or "no" followed by a word with
// no clause break between them. "No problem, booked" is not negative, because a
// comma breaks the "no" from the word after it.
func quoteIsNegative(qt []token) bool {
	for i, t := range qt {
		if t.negating() {
			return true
		}
		if t.word == "no" && i+1 < len(qt) && !qt[i+1].brk {
			return true
		}
	}
	return false
}

// spansSentence reports whether a match that starts at token i and is n tokens
// long crosses a sentence end. Only the breaks between its own tokens count.
func spansSentence(tt []token, i, n int) bool {
	for j := i + 1; j < i+n; j++ {
		if tt[j].sent {
			return true
		}
	}
	return false
}

// sameWords reports whether the words of a and b are equal, in order.
func sameWords(a, b []token) bool {
	for i := range b {
		if a[i].word != b[i].word {
			return false
		}
	}
	return true
}

// negatedAt reports whether the occurrence that starts at token i is negated.
// Two rules apply:
//   - A negating token (not, cannot, can't, unsure, whether, ...) anywhere
//     after the last clause break and before the quote negates it. A clause
//     break is a comma, sentence or clause punctuation, or a dash. So "I don't
//     think your table for two is booked" is negated, but "Don't worry, your
//     table for two is booked" is not, because the comma ends the clause.
//   - "no" negates only when it is the word immediately before the quote, with
//     no clause break between them. "No problem, booked a table for two" is
//     therefore a confirmation, and "No, booked..." is not negated either.
//
// This is a documented heuristic, not a parser. Within a clause, any negation
// word counts, even one that does not negate the quote, so the rule fails safe.
func negatedAt(tt []token, i int) bool {
	for j := i - 1; j >= 0; j-- {
		if tt[j+1].clause {
			// A clause break sits between tt[j] and tt[j+1], so tt[j] is outside
			// the clause of the quote.
			break
		}
		if tt[j].negating() {
			return true
		}
	}
	return i >= 1 && tt[i-1].word == "no" && !tt[i].brk
}

func criterionOf(e Evidence) int {
	if e.Criterion == nil {
		return 0
	}
	return *e.Criterion
}

func transcriptCodes(codes []string, t Transcript) []string {
	if !t.Started {
		return appendCode(codes, CodeTranscriptUnavailable)
	}
	if !t.Finalized {
		codes = appendCode(codes, CodeTranscriptIncomplete)
	}
	if !hasCalleeFinal(t.Turns) {
		codes = appendCode(codes, CodeNoConversation)
	}
	return codes
}

func hasCalleeFinal(turns []Turn) bool {
	for _, t := range turns {
		if t.Speaker == "callee" && t.Finality == "final" {
			return true
		}
	}
	return false
}

// notAnsweredCode maps the effective reason of a call that never connected. The
// three callee outcomes have their own code. Every other value, including
// canceled, hangup, time_limit, local_hangup, unknown, dial and setup failures,
// and media_failed, is call_failed. Values come from internal/call and the
// production writers (see storage.CallEventHangup and storage.CallEventFailed).
func notAnsweredCode(reason string) string {
	switch reason {
	case string(call.ReasonNoAnswer):
		return CodeNoAnswer
	case string(call.ReasonBusy):
		return CodeBusy
	case string(call.ReasonRejected):
		return CodeRejected
	}
	return CodeCallFailed
}

func callEventEvidence(refs []CallEventRef) []Evidence {
	out := make([]Evidence, 0, len(refs))
	for _, r := range refs {
		out = append(out, Evidence{Kind: KindCallEvent, Seq: r.Seq, Code: r.Code, Fidelity: r.Fidelity})
	}
	return out
}

// finish assembles the result, bounding evidence to MaxEvidence. Technical
// call events come first, then verified transcript evidence.
//
// Verified transcript evidence is kept on unknown results too, so a reader can
// see which references were checked. This is a documented design choice (see
// docs/operations/api.md, Evidence semantics). The result stays unknown.
func finish(o Outcome, src Source, codes []string, call, verified []Evidence) Result {
	ev := make([]Evidence, 0, len(call)+len(verified))
	ev = append(ev, call...)
	ev = append(ev, verified...)
	if len(ev) > MaxEvidence {
		ev = ev[:MaxEvidence]
		codes = appendCode(codes, CodeEvidenceTruncated)
	}
	if codes == nil {
		codes = []string{}
	}
	return Result{Outcome: o, Source: src, ReasonCodes: codes, Evidence: ev}
}

// lastValid returns the last assessment with a known outcome and reason.
func lastValid(as []Assessment) (Assessment, bool) {
	for i := len(as) - 1; i >= 0; i-- {
		if validOutcome(as[i].Outcome) && validReason(as[i].Reason) {
			return as[i], true
		}
	}
	return Assessment{}, false
}

func validOutcome(s string) bool {
	switch Outcome(s) {
	case Success, Partial, Failed, Unknown:
		return true
	}
	return false
}

func validReason(s string) bool {
	switch s {
	case ReasonConfirmed, ReasonAlternativeAccepted, ReasonPartiallyFulfilled,
		ReasonCalleeRefused, ReasonCalleeUnable, ReasonVoicemail, ReasonWrongParty, ReasonOther:
		return true
	}
	return false
}

func successReason(s string) bool {
	return s == ReasonConfirmed || s == ReasonAlternativeAccepted
}

// appendCode adds code once, keeping first-seen order.
func appendCode(codes []string, code string) []string {
	if code == "" || slices.Contains(codes, code) {
		return codes
	}
	return append(codes, code)
}
