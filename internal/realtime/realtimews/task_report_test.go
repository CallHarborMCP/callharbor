package realtimews

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/outcome"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// The declared tool is defined once, in internal/outcome. These tests check that
// session.update carries exactly that definition, with auto tool choice.
func TestSessionUpdateDeclaresOnlyTheReportTool(t *testing.T) {
	b, err := SessionUpdate(cfg("wss://api.example/v1/realtime"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Session struct {
			Tools      []map[string]any `json:"tools"`
			ToolChoice string           `json:"tool_choice"`
		} `json:"session"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Session.Tools) != 1 || m.Session.ToolChoice != "auto" {
		t.Fatalf("tools = %d, tool_choice = %q; want one tool and auto", len(m.Session.Tools), m.Session.ToolChoice)
	}
	tool := m.Session.Tools[0]
	if tool["type"] != "function" || tool["name"] != outcome.ReportToolName {
		t.Fatalf("tool = %v", tool)
	}
	// The declared schema is minimal: only the keywords the endpoint is documented
	// to accept. Strict and closed-object rules are enforced by ParseReport, so a
	// rejected keyword cannot break call setup (R19).
	params, _ := tool["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("parameters type = %v, want object", params["type"])
	}
	for _, k := range []string{"strict", "additionalProperties", "maxLength", "maxItems", "minimum", "maximum"} {
		if _, present := params[k]; present {
			t.Fatalf("parameters carry %q; the declared schema must stay minimal", k)
		}
	}
	// The wire form equals the single definition, so there is no second copy.
	want, _ := json.Marshal(outcome.ReportTool())
	got, _ := json.Marshal(tool)
	if string(want) != string(got) {
		t.Fatalf("declared tool differs from outcome.ReportTool()")
	}
}

// The upstream example parses to EventTaskReport only for the report tool.
func TestFunctionCallDoneParsesOnlyTheReportTool(t *testing.T) {
	raw := upstream(t, "response.function_call_arguments.done")
	ev, err := ParseServerEvent(raw)
	if err != nil {
		t.Fatalf("documented example rejected: %v", err)
	}
	// The documented example calls get_weather, which is not ours: ignored.
	if ev.Kind != realtime.EventIgnored {
		t.Fatalf("foreign function call kind = %v, want ignored", ev.Kind)
	}

	report := strings.Replace(string(raw), `"name": "get_weather"`, `"name": "`+outcome.ReportToolName+`"`, 1)
	report = strings.Replace(report, `"arguments": "{\"location\": \"San Francisco\"}"`,
		`"arguments": "{\"outcome\":\"failed\",\"reason\":\"other\",\"criteria\":[],\"constraints\":[]}"`, 1)
	ev, err = ParseServerEvent([]byte(report))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != realtime.EventTaskReport || ev.FunctionCallID != "call_001" || ev.ReportInvalid {
		t.Fatalf("report event = %+v", ev)
	}
	if !strings.Contains(ev.Arguments, `"outcome":"failed"`) {
		t.Fatalf("arguments not carried: %q", ev.Arguments)
	}
}

func TestOversizedOrMissingReportIsFlaggedNotAnError(t *testing.T) {
	big := strings.Repeat("a", realtime.MaxTaskReportArgs+1)
	oversized := `{"type":"response.function_call_arguments.done","event_id":"e1","response_id":"r","item_id":"i","call_id":"call_9","name":"` +
		outcome.ReportToolName + `","arguments":"` + big + `"}`
	ev, err := ParseServerEvent([]byte(oversized))
	if err != nil {
		t.Fatalf("oversized report ended the parse: %v", err)
	}
	if ev.Kind != realtime.EventTaskReport || !ev.ReportInvalid || ev.Arguments != "" {
		t.Fatalf("oversized report = %+v, want flagged with no arguments kept", ev)
	}

	noArgs := `{"type":"response.function_call_arguments.done","event_id":"e2","call_id":"call_10","name":"` + outcome.ReportToolName + `"}`
	ev, err = ParseServerEvent([]byte(noArgs))
	if err != nil || !ev.ReportInvalid {
		t.Fatalf("report without arguments: err=%v ev=%+v, want flagged", err, ev)
	}

	noCall := `{"type":"response.function_call_arguments.done","event_id":"e3","name":"` + outcome.ReportToolName + `","arguments":"{}"}`
	ev, err = ParseServerEvent([]byte(noCall))
	if err != nil || !ev.ReportInvalid || ev.FunctionCallID != "" {
		t.Fatalf("report without call_id: err=%v ev=%+v, want flagged with no id", err, ev)
	}
}

// The function call item and its output must never be caller audio, never be
// requested as out-of-band transcription, and never change the coordinator.
// Both are fed through the same coordinator in one pass.
func TestFunctionCallItemsCoexistWithOutOfBand(t *testing.T) {
	st := newOOBState(time.Now)
	fnItem := []byte(`{"type":"conversation.item.done","item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"report_task_outcome","arguments":"{}"}}`)
	fnOutput := []byte(`{"type":"conversation.item.added","item":{"id":"fo_1","type":"function_call_output","status":"completed","call_id":"call_1","output":"{\"status\":\"recorded\"}"}}`)

	for _, c := range []struct {
		name string
		raw  []byte
	}{{"conversation.item.done", fnItem}, {"conversation.item.added", fnOutput}} {
		if id, ok := callerAudioItem(c.raw); ok || id != "" {
			t.Fatalf("%s treated as caller audio: %q", c.name, id)
		}
		sends, consumed, err := st.handleOOB(c.name, c.raw)
		if err != nil || consumed || len(sends) != 0 {
			t.Fatalf("%s: sends=%d consumed=%v err=%v; want untouched", c.name, len(sends), consumed, err)
		}
	}
	if len(st.pending) != 0 || len(st.fifo) != 0 || len(st.requested) != 0 {
		t.Fatalf("function items changed out-of-band state: pending=%d fifo=%d requested=%d",
			len(st.pending), len(st.fifo), len(st.requested))
	}

	// A real caller audio item still works alongside the function items.
	caller := []byte(`{"type":"conversation.item.added","item":{"id":"item_caller","type":"message","status":"completed","role":"user","content":[{"type":"input_audio"}]}}`)
	sends, _, err := st.handleOOB("conversation.item.added", caller)
	if err != nil || len(sends) != 1 || len(st.pending) != 1 {
		t.Fatalf("caller audio beside function items: sends=%d pending=%d err=%v", len(sends), len(st.pending), err)
	}
}

// The ack is one conversation.item.create of a function output carrying the call
// id and the recorded status. No response.create may follow it, so the model
// does not speak in reply. The client messages the fake server receives are
// checked, not a copy of the expected shape.
func TestAckTaskReportSendsFunctionOutputOnly(t *testing.T) {
	var mu sync.Mutex
	var seen []map[string]any
	f := &fakeRealtime{onClient: func(_ context.Context, _ *websocket.Conn, m map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, m)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Connect(ctx, cfg(startFake(t, f)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.AckTaskReport(ctx, "", "recorded"); err == nil {
		t.Fatal("empty call id must be refused before anything is sent")
	}
	if err := s.AckTaskReport(ctx, "call_001", "bogus"); err == nil {
		t.Fatal("an unknown ack status must be refused")
	}
	if err := s.AckTaskReport(ctx, "call_001", "recorded"); err != nil {
		t.Fatal(err)
	}
	// Wait until the fake server has read the ack, not just the session.update.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		acked := false
		for _, m := range seen {
			if m["type"] == "conversation.item.create" {
				acked = true
			}
		}
		mu.Unlock()
		if acked || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	// The session.update is also seen; only the messages after it count.
	var after []map[string]any
	started := false
	for _, m := range seen {
		if m["type"] == "session.update" {
			started = true
			continue
		}
		if started {
			after = append(after, m)
		}
	}
	for _, m := range after {
		if m["type"] == "response.create" {
			t.Fatalf("ack started a response: %v", m)
		}
	}
	if len(after) != 1 {
		t.Fatalf("messages after session.update = %d, want exactly the ack", len(after))
	}
	m := after[0]
	item, _ := m["item"].(map[string]any)
	if m["type"] != "conversation.item.create" || item["type"] != "function_call_output" ||
		item["call_id"] != "call_001" || item["output"] != `{"status":"recorded"}` {
		t.Fatalf("ack = %v", m)
	}
}

// An invalid report is acknowledged with the invalid status, as a function output
// only. The output names the status so the model is not left waiting.
func TestAckTaskReportInvalidStatusOutput(t *testing.T) {
	var mu sync.Mutex
	var seen []map[string]any
	f := &fakeRealtime{onClient: func(_ context.Context, _ *websocket.Conn, m map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, m)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Connect(ctx, cfg(startFake(t, f)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.AckTaskReport(ctx, "call_bad", "invalid"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	last := seen[len(seen)-1]
	item, _ := last["item"].(map[string]any)
	if last["type"] != "conversation.item.create" || item["call_id"] != "call_bad" || item["output"] != `{"status":"invalid"}` {
		t.Fatalf("invalid ack = %v", last)
	}
}

// N4: arguments that are the right size but not a valid report (not JSON, an
// unknown field, a missing field, a bad enum) are flagged ReportInvalid at the
// adapter. The event is still delivered with the call id, so the engine can ack
// it as invalid. Each case is a real malformed body, not a hand-set flag.
func TestMalformedReportArgumentsAreFlaggedInvalid(t *testing.T) {
	for _, args := range []string{
		`not json at all`,
		`{"outcome":"success","reason":"confirmed","criteria":[],"constraints":[],"surprise":1}`,
		`{"outcome":"success","reason":"confirmed","criteria":[]}`,
		`{"outcome":"maybe","reason":"confirmed","criteria":[],"constraints":[]}`,
	} {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		raw := `{"type":"response.function_call_arguments.done","event_id":"e4","response_id":"r","item_id":"i","call_id":"call_bad","name":"` +
			outcome.ReportToolName + `","arguments":` + string(b) + `}`
		ev, err := ParseServerEvent([]byte(raw))
		if err != nil {
			t.Fatalf("%q: malformed report ended the parse: %v", args, err)
		}
		if ev.Kind != realtime.EventTaskReport || ev.FunctionCallID != "call_bad" {
			t.Fatalf("%q: event = %+v, want a task report with its call id", args, ev)
		}
		if !ev.ReportInvalid {
			t.Fatalf("%q: malformed arguments were not flagged invalid: %+v", args, ev)
		}
	}
}

// N6: the tool description tells the model to call once, when the outcome is
// final, before it ends the call.
func TestReportToolDescriptionSaysCallOnceWhenFinal(t *testing.T) {
	d, _ := outcome.ReportTool()["description"].(string)
	const want = "Call exactly once, when the outcome is final, before ending the call."
	if !strings.HasPrefix(d, want) {
		t.Fatalf("description = %q, want it to start with %q", d, want)
	}
}
