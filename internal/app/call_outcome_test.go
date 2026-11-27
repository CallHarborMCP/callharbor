package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/observability"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
)

// outcomePoll is the dial worker's poll in outcome tests. Evaluation runs on
// the worker tick, so a short poll makes it land promptly without a wake.
const outcomePoll = 20 * time.Millisecond

// outcomeGrace shortens the outcome grace period in these tests, so a call that
// ended without a transcript is judged promptly. Production keeps 2 minutes
// (callworker.OutcomeGrace). The grace itself is covered by storage readiness
// tests.
const outcomeGrace = 50 * time.Millisecond

// outcomeCall is one call driven through the real app wiring up to a live
// media run. The fakes are the only stand-ins: Telnyx REST and the AI endpoint.
type outcomeCall struct {
	t      *testing.T
	env    *callTestEnv
	tel    *fakeTelnyx
	ai     *fakeAI
	h      http.Header
	key    string
	callID string
	media  *telnyxfake.Stream
}

// startOutcomeCall creates a call with the given brief and drives it to a live
// media run: dial, initiated, answered, stream start and the first caller audio.
func startOutcomeCall(t *testing.T, protocol realtime.Protocol, key string, logger *slog.Logger, criteria, constraints []string) *outcomeCall {
	t.Helper()
	tel := newFakeTelnyx(t, "v3:ctl-"+key)
	ai := newFakeAI(t, protocol)
	env := startCallAppTuned(t, tel, nil, nil, logger, outcomePoll, outcomeGrace)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, protocol, ai.endpoint())

	body := map[string]any{"profile_id": tpID, "to": "+14155550111", "objective": "Book a table for two"}
	if len(criteria) > 0 {
		body["success_criteria"] = criteria
	}
	if len(constraints) > 0 {
		body["constraints"] = constraints
	}
	status, _, raw := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", body, withIdempotency(h, key))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, raw)
	}
	callID, _ := decodeObject(t, raw)["call_id"].(string)
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-init-"+key, "v3:ctl-"+key)
	env.signedWebhook(callID, "call.answered", "evt-ans-"+key, "v3:ctl-"+key)
	media, err := env.signedMediaDial(env.dialedStreamURL())
	if err != nil {
		t.Fatalf("media dial: %v", err)
	}
	sendStart(t, media, "v3:ctl-"+key)
	sendMedia(t, media, "AAAAAAAAAAA=")
	waitEvents(t, ai, 2)
	return &outcomeCall{t: t, env: env, tel: tel, ai: ai, h: h, key: key, callID: callID, media: media}
}

// calleeSpeaks sends one final callee utterance on the provider. Realtime
// callers go through the out-of-band transcription answer; live callers arrive
// as input transcript deltas.
func (c *outcomeCall) calleeSpeaks(text string) {
	c.t.Helper()
	if c.ai.protocol == realtime.ProtocolLive {
		c.ai.sendRaw(c.t, map[string]any{"type": "session.input_transcript.delta", "event_id": "ev_i_" + c.key,
			"delta": text, "start_ms": 1200, "end_ms": 1900})
		return
	}
	c.ai.sendRaw(c.t, map[string]any{"type": "input_audio_buffer.speech_started", "event_id": "ev_s_" + c.key,
		"audio_start_ms": 1200, "item_id": "msg_2"})
	c.ai.sendRaw(c.t, map[string]any{"type": "conversation.item.added", "event_id": "ev_add_" + c.key, "item": map[string]any{
		"id": "msg_2", "type": "message", "status": "completed", "role": "user",
		"content": []map[string]any{{"type": "input_audio"}}}})
	answerOutOfBand(c.t, c.ai, "msg_2", text)
}

// report sends the voice session's report_task_outcome call with the given
// JSON arguments, as the realtime provider would after the tool call finishes.
func (c *outcomeCall) report(args string) {
	c.t.Helper()
	c.ai.sendRaw(c.t, map[string]any{"type": "response.function_call_arguments.done", "event_id": "ev_fc_" + c.key,
		"response_id": "resp_r_" + c.key, "item_id": "fc_" + c.key, "output_index": 0,
		"call_id": "call_" + c.key, "name": "report_task_outcome", "arguments": args})
}

// finish closes the media stream, sends the Telnyx hangup that ends the call,
// waits for the transcript to finalize and the call to reach wantStatus, then
// returns the REST call once an outcome is set. A closed media stream alone does
// not end the call: the provider's hangup does.
func (c *outcomeCall) finish(wantStatus string) map[string]any {
	c.t.Helper()
	_ = c.media.Close()
	c.env.signedHangup(c.callID, "evt-hup-"+c.key, "v3:ctl-"+c.key, "normal_clearing")
	waitTranscriptFinalized(c.t, c.env.dataDir, c.callID)
	waitForCallStatus(c.t, c.env, c.h, c.callID, wantStatus)
	return waitTaskOutcome(c.t, c.env, c.h, c.callID)
}

// waitTaskOutcome polls the owner's REST call until the worker has recorded an
// outcome, or fails after a bounded wait.
func waitTaskOutcome(t *testing.T, env *callTestEnv, h http.Header, callID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID, nil, h)
		if status == http.StatusOK {
			m := decodeObject(t, body)
			if d, _ := m["details"].(map[string]any); m["task_outcome"] != nil && d["task_assessment"] != nil {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("call %s never recorded a task outcome", callID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// calleeSeq returns the seq of the first final callee turn the owner's REST
// transcript shows. Evidence must point at this seq.
func calleeSeq(t *testing.T, env *callTestEnv, h http.Header, callID string) int {
	t.Helper()
	status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID+"/transcript", nil, h)
	if status != http.StatusOK {
		t.Fatalf("owner transcript = %d %s", status, body)
	}
	var page transcriptPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	for _, turn := range page.Turns {
		if turn.Speaker == "callee" {
			return turn.Seq
		}
	}
	t.Fatalf("no callee turn in %+v", page.Turns)
	return 0
}

// assertOutcome checks one REST or MCP call map: the task outcome, the
// technical status (independent of the outcome), and that every wanted reason
// code is present. It returns the assessment's evidence list.
func assertOutcome(t *testing.T, m map[string]any, wantOutcome, wantStatus string, codes ...string) []map[string]any {
	t.Helper()
	if got, _ := m["task_outcome"].(string); got != wantOutcome {
		t.Fatalf("task_outcome = %v, want %s (call %v)", m["task_outcome"], wantOutcome, m)
	}
	if got, _ := m["technical_status"].(string); got != wantStatus {
		t.Fatalf("technical_status = %v, want %s", m["technical_status"], wantStatus)
	}
	// The assessment is under details: task_outcome is the enum, and the
	// reasoning is the nullable details.task_assessment (see the API contract).
	details, _ := m["details"].(map[string]any)
	ta, ok := details["task_assessment"].(map[string]any)
	if !ok {
		t.Fatalf("details.task_assessment = %v, want an object", details["task_assessment"])
	}
	got, _ := ta["reason_codes"].([]any)
	for _, want := range codes {
		if !anyEquals(got, want) {
			t.Fatalf("reason_codes = %v, want %s among them", got, want)
		}
	}
	raw, _ := ta["evidence"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

// anyEquals reports whether list holds the string want.
func anyEquals(list []any, want string) bool {
	return slices.ContainsFunc(list, func(v any) bool { return v == want })
}

// evidenceOfKind returns the evidence entries of one kind.
func evidenceOfKind(ev []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, e := range ev {
		if e["kind"] == kind {
			out = append(out, e)
		}
	}
	return out
}

// assertCalleeEvidence checks that the call's transcript evidence points at the
// callee turn the REST transcript shows, names its speaker, and carries a
// criterion index when one is claimed.
func (c *outcomeCall) assertCalleeEvidence(ev []map[string]any, wantCriterion int) {
	c.t.Helper()
	turns := evidenceOfKind(ev, "transcript_turn")
	if len(turns) == 0 {
		c.t.Fatalf("no transcript_turn evidence in %v", ev)
	}
	seq := calleeSeq(c.t, c.env, c.h, c.callID)
	for _, e := range turns {
		if e["seq"] != float64(seq) || e["speaker"] != "callee" {
			c.t.Fatalf("transcript evidence %v does not point at callee seq %d", e, seq)
		}
		if wantCriterion > 0 && e["criterion"] != float64(wantCriterion) {
			c.t.Fatalf("evidence criterion = %v, want %d", e["criterion"], wantCriterion)
		}
	}
}

// assertReportAck checks the voice session saw exactly the function output for
// this report, and that the report never made the model speak (no response.create
// except the out-of-band transcription).
func (c *outcomeCall) assertReportAck() {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(c.ai.toolOutputsSeen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	outs := c.ai.toolOutputsSeen()
	if len(outs) != 1 {
		c.t.Fatalf("function outputs = %v, want exactly one ack", outs)
	}
	if outs[0]["call_id"] != "call_"+c.key || outs[0]["output"] != `{"status":"recorded"}` {
		c.t.Fatalf("ack = %v", outs[0])
	}
	if n := c.ai.plainResponseCount(); n != 0 {
		c.t.Fatalf("report made the model speak: %d plain response.create", n)
	}
}

// TestOutcomeConfirmedAppointment: a callee confirms the booking, the criterion
// is quoted from the callee turn, and the outcome is success.
func TestOutcomeConfirmedAppointment(t *testing.T) {
	c := startOutcomeCall(t, realtime.ProtocolRealtime, "out-ok", observability.NewLogger(io.Discard, slog.LevelError),
		[]string{"A table for two is booked"}, []string{"Do not share payment details"})
	c.calleeSpeaks("Your table for two is booked for 7pm.")
	c.report(`{"outcome":"success","reason":"confirmed","criteria":[{"index":1,"met":true,"evidence_quote":"table for two is booked"}],"constraints":[{"index":1,"respected":true}]}`)
	c.assertReportAck()
	m := c.finish("ended")
	ev := assertOutcome(t, m, "success", "ended", "confirmed")
	c.assertCalleeEvidence(ev, 1)
}

// TestOutcomeNoAnswerIsFailedWithCallEvidence: the callee never answers and the
// call ends with no_answer. The outcome is failed, the technical status is
// failed, and the evidence is the call event, not a transcript turn.
func TestOutcomeNoAnswerIsFailedWithCallEvidence(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-noans")
	ai := newFakeAI(t, realtime.ProtocolRealtime)
	env := startCallAppTuned(t, tel, nil, nil, observability.NewLogger(io.Discard, slog.LevelError), outcomePoll, outcomeGrace)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, ai.endpoint())

	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Book a table for two",
		"success_criteria": []string{"A table for two is booked"},
	}, withIdempotency(h, "out-noans"))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-init-noans", "v3:ctl-noans")
	env.signedHangup(callID, "evt-hup-noans", "v3:ctl-noans", "no_answer")
	waitForCallStatus(t, env, h, callID, "failed")

	if n := len(ai.snapshot()); n != 0 {
		t.Fatalf("no-answer call opened a provider session: %v", ai.snapshot())
	}
	m := waitTaskOutcome(t, env, h, callID)
	ev := assertOutcome(t, m, "failed", "failed", "no_answer")
	calls := evidenceOfKind(ev, "call_event")
	if len(calls) == 0 || calls[0]["seq"] == nil {
		t.Fatalf("no call_event evidence in %v", ev)
	}
	if n := len(evidenceOfKind(ev, "transcript_turn")); n != 0 {
		t.Fatalf("no-answer call carries %d transcript evidence", n)
	}
}

// TestOutcomeAlternativeAcceptedWithinConstraints: the callee offers another
// time, the caller accepts it within the constraints, and the criterion is met
// by the callee's own words. The outcome is success with alternative_accepted.
func TestOutcomeAlternativeAcceptedWithinConstraints(t *testing.T) {
	c := startOutcomeCall(t, realtime.ProtocolRealtime, "out-alt", observability.NewLogger(io.Discard, slog.LevelError),
		[]string{"A table for two is booked"}, []string{"Only after 7pm"})
	c.calleeSpeaks("We can offer a table for two at 8pm instead.")
	c.report(`{"outcome":"success","reason":"alternative_accepted","criteria":[{"index":1,"met":true,"evidence_quote":"table for two at 8pm"}],"constraints":[{"index":1,"respected":true}]}`)
	c.assertReportAck()
	m := c.finish("ended")
	ev := assertOutcome(t, m, "success", "ended", "alternative_accepted")
	c.assertCalleeEvidence(ev, 1)
}

// TestOutcomePartialResult: the callee confirms one of two requested items. The
// criterion for the confirmed item is met, the other is not, and the outcome is
// partial. A partial claim does not add criteria_unmet; only a success claim
// with some criteria met does. The test asserts partially_fulfilled only.
func TestOutcomePartialResult(t *testing.T) {
	c := startOutcomeCall(t, realtime.ProtocolRealtime, "out-part", observability.NewLogger(io.Discard, slog.LevelError),
		[]string{"A table for two is booked", "A window seat is reserved"}, nil)
	c.calleeSpeaks("Your table for two is booked, but we have no window seats.")
	c.report(`{"outcome":"partial","reason":"partially_fulfilled","criteria":[{"index":1,"met":true,"evidence_quote":"table for two is booked"},{"index":2,"met":false,"evidence_quote":""}],"constraints":[]}`)
	c.assertReportAck()
	m := c.finish("ended")
	// A partial claim carries its reason; criteria_unmet is only added when a
	// success claim falls short. The unmet criterion has an empty quote, so it
	// adds no evidence_unverified: the codes are exactly [partially_fulfilled].
	ev := assertOutcome(t, m, "partial", "ended", "partially_fulfilled")
	ta := m["details"].(map[string]any)["task_assessment"].(map[string]any)
	if codes, _ := ta["reason_codes"].([]any); len(codes) != 1 || codes[0] != "partially_fulfilled" {
		t.Fatalf("reason_codes = %v, want exactly [partially_fulfilled]", codes)
	}
	c.assertCalleeEvidence(ev, 1)
}

// TestOutcomeRefusalIsFailed: the callee refuses, and the refusal is quoted as
// the callee's own words against the criterion it defeats. Refusal is failed
// with callee_refused, never success.
func TestOutcomeRefusalIsFailed(t *testing.T) {
	c := startOutcomeCall(t, realtime.ProtocolRealtime, "out-ref", observability.NewLogger(io.Discard, slog.LevelError),
		[]string{"A table for two is booked"}, nil)
	c.calleeSpeaks("Sorry, we are fully booked and cannot take a table for two.")
	c.report(`{"outcome":"failed","reason":"callee_refused","criteria":[{"index":1,"met":false,"evidence_quote":"cannot take a table for two"}],"constraints":[]}`)
	c.assertReportAck()
	m := c.finish("ended")
	ev := assertOutcome(t, m, "failed", "ended", "callee_refused")
	c.assertCalleeEvidence(ev, 1)
	// The criterion is unmet, so the refusal quote is evidence with met:false. It
	// is not a success claim and never counts toward success.
	for _, e := range evidenceOfKind(ev, "transcript_turn") {
		if e["met"] != false {
			t.Fatalf("refusal evidence met = %v, want false: %v", e["met"], e)
		}
	}
}

// TestOutcomeVoicemailIsFailed: the callee's words are a voicemail greeting and
// the session reports voicemail with an outcome-level quote (no criterion). The
// outcome is failed, and the quote is evidence with no criterion and no met.
func TestOutcomeVoicemailIsFailed(t *testing.T) {
	c := startOutcomeCall(t, realtime.ProtocolRealtime, "out-vm", observability.NewLogger(io.Discard, slog.LevelError),
		[]string{"A table for two is booked"}, nil)
	c.calleeSpeaks("The person you are calling is unavailable, please leave a message after the tone.")
	c.report(`{"outcome":"failed","reason":"voicemail","criteria":[],"constraints":[],"evidence_quotes":["please leave a message after the tone"]}`)
	c.assertReportAck()
	m := c.finish("ended")
	ev := assertOutcome(t, m, "failed", "ended", "voicemail")
	turns := evidenceOfKind(ev, "transcript_turn")
	if len(turns) != 1 {
		t.Fatalf("voicemail evidence = %v, want one transcript turn", ev)
	}
	if _, has := turns[0]["criterion"]; has {
		t.Fatalf("outcome-level evidence must carry no criterion: %v", turns[0])
	}
	if _, has := turns[0]["met"]; has {
		t.Fatalf("outcome-level evidence must omit met: %v", turns[0])
	}
}

// TestOutcomeInsufficientEvidence covers the two unknown paths. A success
// report whose quote the transcript does not contain is unverified. A
// conversation with no report at all is assessment_missing.
func TestOutcomeInsufficientEvidence(t *testing.T) {
	t.Run("unverified quote", func(t *testing.T) {
		c := startOutcomeCall(t, realtime.ProtocolRealtime, "out-unv", observability.NewLogger(io.Discard, slog.LevelError),
			[]string{"A table for two is booked"}, nil)
		c.calleeSpeaks("We will think about it.")
		c.report(`{"outcome":"success","reason":"confirmed","criteria":[{"index":1,"met":true,"evidence_quote":"table for two is booked"}],"constraints":[]}`)
		c.assertReportAck()
		m := c.finish("ended")
		assertOutcome(t, m, "unknown", "ended", "evidence_unverified")
	})
	t.Run("no report", func(t *testing.T) {
		c := startOutcomeCall(t, realtime.ProtocolRealtime, "out-nor", observability.NewLogger(io.Discard, slog.LevelError),
			[]string{"A table for two is booked"}, nil)
		c.calleeSpeaks("Your table for two is booked.")
		m := c.finish("ended")
		assertOutcome(t, m, "unknown", "ended", "assessment_missing")
	})
}

// TestOutcomeLiveWSIsUnknownUnsupported: a live_ws call has no voice-session
// self-report channel, so its outcome is unknown with assessment_unsupported,
// even with a transcript. A successful transcript is not evidence of success.
func TestOutcomeLiveWSIsUnknownUnsupported(t *testing.T) {
	c := startOutcomeCall(t, realtime.ProtocolLive, "out-live", observability.NewLogger(io.Discard, slog.LevelError),
		[]string{"A table for two is booked"}, nil)
	c.calleeSpeaks("Your table for two is booked.")
	m := c.finish("ended")
	assertOutcome(t, m, "unknown", "ended", "assessment_unsupported")
}

// TestOutcomeCrossUserAndRedaction: a second user gets the same not_found for
// the call on REST and MCP, list_calls excludes it, and the captured logs carry
// no brief objective, success criterion, quote or transcript text.
func TestOutcomeCrossUserAndRedaction(t *testing.T) {
	const objective = "Book a table for two"
	const criterion = "A table for two is booked"
	const quote = "table for two is booked"
	const calleeText = "Your table for two is booked for 7pm."

	var logs syncBuffer
	tel := newFakeTelnyx(t, "v3:ctl-xu")
	ai := newFakeAI(t, realtime.ProtocolRealtime)
	env := startCallAppTuned(t, tel, nil, nil, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), outcomePoll, outcomeGrace)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, ai.endpoint())
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": objective,
		"success_criteria": []string{criterion},
	}, withIdempotency(h, "out-xu"))
	if status != http.StatusAccepted {
		t.Fatalf("create call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-init-xu", "v3:ctl-xu")
	env.signedWebhook(callID, "call.answered", "evt-ans-xu", "v3:ctl-xu")
	media, err := env.signedMediaDial(env.dialedStreamURL())
	if err != nil {
		t.Fatalf("media dial: %v", err)
	}
	sendStart(t, media, "v3:ctl-xu")
	sendMedia(t, media, "AAAAAAAAAAA=")
	waitEvents(t, ai, 2)
	// The key names the Telnyx control id the dial used (v3:ctl-xu), so the
	// hangup that finish sends carries the same control id as the dial.
	c := &outcomeCall{t: t, env: env, tel: tel, ai: ai, h: h, key: "xu", callID: callID, media: media}
	c.calleeSpeaks(calleeText)
	c.report(`{"outcome":"success","reason":"confirmed","criteria":[{"index":1,"met":true,"evidence_quote":"` + quote + `"}],"constraints":[]}`)
	c.assertReportAck()
	m := c.finish("ended")
	assertOutcome(t, m, "success", "ended", "confirmed")

	// REST: a foreign user gets the same not_found as an unknown id.
	mallory := createUser(t, env.admin, "mallory")
	mh := mallory.headers(false)
	status, _, body = send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID, nil, mh)
	if status != http.StatusNotFound {
		t.Fatalf("foreign REST get_call = %d %s, want 404", status, body)
	}
	status, _, unknownBody := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/call_missing", nil, mh)
	if status != http.StatusNotFound || string(unknownBody) != string(body) {
		t.Fatalf("foreign not_found differs from unknown id: %s vs %s", body, unknownBody)
	}

	// MCP: the foreign user's get_call is refused with not_found, and list_calls
	// does not include the call.
	redirect := callbackURL(t)
	oe := env.oauthView()
	cid := oe.register(redirect)
	mtok := oe.grant(mallory, cid, redirect, "mcp:read").AccessToken
	msess := oe.sdkClient(t, mtok)
	if code := toolErrorCode(t, callTool(t, msess, "get_call", map[string]any{"call_id": callID})); code != "not_found" {
		t.Fatalf("foreign MCP get_call code = %q, want not_found", code)
	}
	listed := structuredMap(t, callTool(t, msess, "list_calls", map[string]any{}))
	for _, x := range listed["calls"].([]any) {
		if x.(map[string]any)["call_id"] == callID {
			t.Fatalf("foreign list_calls includes call %s", callID)
		}
	}

	// Owner MCP get_call reports the same assessment as REST.
	ownerTok := oe.grant(env.admin, cid, redirect, "mcp:read").AccessToken
	mo := structuredMap(t, callTool(t, oe.sdkClient(t, ownerTok), "get_call", map[string]any{"call_id": callID}))
	assertOutcome(t, mo, "success", "ended", "confirmed")

	// Log redaction: nothing the call handled may appear in the captured logs.
	for _, secret := range []string{objective, criterion, quote, calleeText} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs carry call content %q", secret)
		}
	}
}

// syncBuffer is a log sink safe for the app's goroutines to write to while the
// test reads it. It keeps every log line for the redaction check.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestOutcomeZeroCriteriaSuccessWithEvidenceQuote: a brief with no success
// criteria can succeed on one verified outcome-level quote. The report travels
// the real wire path, and the result is success with the quote as evidence and
// no criterion on it.
func TestOutcomeZeroCriteriaSuccessWithEvidenceQuote(t *testing.T) {
	c := startOutcomeCall(t, realtime.ProtocolRealtime, "out-zero", observability.NewLogger(io.Discard, slog.LevelError),
		nil, nil)
	c.calleeSpeaks("Yes, you are all set for Tuesday at three.")
	c.report(`{"outcome":"success","reason":"confirmed","criteria":[],"constraints":[],"evidence_quotes":["you are all set for tuesday at three"]}`)
	c.assertReportAck()
	m := c.finish("ended")
	ev := assertOutcome(t, m, "success", "ended", "confirmed")
	turns := evidenceOfKind(ev, "transcript_turn")
	if len(turns) != 1 {
		t.Fatalf("zero-criteria success evidence = %v, want one transcript turn", ev)
	}
	if _, has := turns[0]["criterion"]; has {
		t.Fatalf("outcome-level evidence must carry no criterion: %v", turns[0])
	}
}
