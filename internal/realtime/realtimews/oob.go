package realtimews

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// Out-of-band caller transcription. For each caller audio item the session
// sends one response.create with conversation "none" that references the item
// and asks the configured voice model for a verbatim, text-only transcription.
// That response is not part of the call conversation: its audio and agent
// transcript events are never forwarded, and its text is reported only as
// caller transcript. Input transcription (audio.input.transcription) is never
// set, so no separately billed transcription model is involved.
const (
	oobPurposeKey       = "callharbor_purpose"
	oobPurpose          = "caller_transcript"
	oobItemKey          = "callharbor_item_id"
	oobEventPrefix      = "ch_oob_"
	oobCancelPrefix     = "ch_oob_cancel_"
	oobMaxOutputTokens  = 1024
	maxOOBTracked       = 4096
	MaxPendingOutOfBand = 8
	OutOfBandTimeout    = 30 * time.Second
	oobPrompt           = "Transcribe only the referenced caller audio, verbatim, in the language spoken. " +
		"Do not translate, answer, summarize or add anything. If there is no speech, output nothing."
)

// itemIDPattern is the item_reference id pattern of the Realtime client schema.
var itemIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// oobRequest is one out-of-band transcription from send to a terminal state.
type oobRequest struct {
	itemID     string
	eventID    string
	responseID string
	seq        int
	started    time.Time
	// final is set once the final text was emitted from output_text.done.
	final bool
}

// oobState is owned by one Session and touched only from Next, the single
// downlink reader, so it needs no lock.
type oobState struct {
	now         func() time.Time
	seq         int
	cancelSeq   int
	unsupported bool
	// requested is the bounded set of item ids already requested or failed.
	requested     map[string]bool
	requestedList []string
	// pending holds every in-flight request, keyed by client event id.
	pending map[string]*oobRequest
	// byResponse maps a correlated response id to its request.
	byResponse map[string]*oobRequest
	// retired holds response ids that reached a terminal state or timed out.
	// Their late events are dropped, never delivered as call audio or text.
	retired     map[string]bool
	retiredList []string
	// fifo holds normalized events that Next returns before reading again.
	fifo []realtime.Event
	// cancel holds targeted response.cancel messages for our own responses,
	// sent by Next through the bounded write.
	cancel [][]byte
}

func newOOBState(now func() time.Time) *oobState {
	return &oobState{
		now:        now,
		requested:  map[string]bool{},
		pending:    map[string]*oobRequest{},
		byResponse: map[string]*oobRequest{},
		retired:    map[string]bool{},
	}
}

// localFail is a failure decided by CallHarbor, not by a provider message.
func localFail(itemID, code string) realtime.Event {
	return realtime.Event{Kind: realtime.EventCallerTranscriptFailed, RawType: "callharbor.out_of_band", ItemID: itemID, ErrCode: code, OutOfBand: true}
}

func (o *oobState) pop() (realtime.Event, bool) {
	if len(o.fifo) == 0 {
		return realtime.Event{}, false
	}
	ev := o.fifo[0]
	o.fifo = o.fifo[1:]
	return ev, true
}

func (o *oobState) remember(itemID string) {
	if len(o.requestedList) >= maxOOBTracked {
		delete(o.requested, o.requestedList[0])
		o.requestedList = o.requestedList[1:]
	}
	o.requested[itemID] = true
	o.requestedList = append(o.requestedList, itemID)
}

func (o *oobState) retire(responseID string) {
	if responseID == "" {
		return
	}
	if len(o.retiredList) >= maxOOBTracked {
		delete(o.retired, o.retiredList[0])
		o.retiredList = o.retiredList[1:]
	}
	o.retired[responseID] = true
	o.retiredList = append(o.retiredList, responseID)
}

// finish removes a request from every table once it is terminal.
func (o *oobState) finish(req *oobRequest) {
	delete(o.pending, req.eventID)
	if req.responseID != "" {
		delete(o.byResponse, req.responseID)
		o.retire(req.responseID)
	}
}

// request decides what to do for a caller audio item. It queues any failure
// event and returns the response.create to send, if one is needed.
func (o *oobState) request(itemID string) ([]byte, error) {
	if o.requested[itemID] {
		return nil, nil
	}
	o.remember(itemID)
	switch {
	case !itemIDPattern.MatchString(itemID):
		o.fifo = append(o.fifo, localFail(itemID, "item_id_invalid"))
		return nil, nil
	case o.unsupported:
		o.fifo = append(o.fifo, localFail(itemID, "out_of_band_unsupported"))
		return nil, nil
	case len(o.pending) >= MaxPendingOutOfBand:
		o.fifo = append(o.fifo, localFail(itemID, "pending_limit"))
		return nil, nil
	}
	o.seq++
	eventID := fmt.Sprintf("%s%d", oobEventPrefix, o.seq)
	o.pending[eventID] = &oobRequest{itemID: itemID, eventID: eventID, seq: o.seq, started: o.now()}
	return json.Marshal(map[string]any{
		"type":     "response.create",
		"event_id": eventID,
		"response": map[string]any{
			"conversation":      "none",
			"output_modalities": []string{"text"},
			"instructions":      oobPrompt,
			"max_output_tokens": oobMaxOutputTokens,
			"metadata":          map[string]string{oobPurposeKey: oobPurpose, oobItemKey: itemID},
			"input":             []map[string]string{{"type": "item_reference", "id": itemID}},
		},
	})
}

// expire fails requests older than OutOfBandTimeout, oldest first. It runs when
// Next processes a message, so no socket read is cancelled to implement a timer.
// A request whose final text was already delivered is finished silently: the
// transcription is complete, and only its terminal response.done is missing.
func (o *oobState) expire() {
	now := o.now()
	var due []*oobRequest
	for _, req := range o.pending {
		if now.Sub(req.started) >= OutOfBandTimeout {
			due = append(due, req)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].seq < due[j].seq })
	for _, req := range due {
		o.finish(req)
		if req.final {
			continue
		}
		o.fifo = append(o.fifo, localFail(req.itemID, "timeout"))
	}
}

// providerCode bounds a provider error code before it is stored: at most 64
// bytes of [A-Za-z0-9_.-], every other byte replaced by '_'.
func providerCode(code string) string {
	if code == "" {
		return "unspecified"
	}
	b := []byte(code)
	if len(b) > 64 {
		b = b[:64]
	}
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
		default:
			b[i] = '_'
		}
	}
	return string(b)
}

// callerAudioItem reports whether a conversation item is a completed user
// message carrying input audio. Only such items are transcribed.
func callerAudioItem(raw []byte) (string, bool) {
	var it struct {
		Item struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Status  string `json:"status"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &it) != nil {
		return "", false
	}
	x := it.Item
	if x.Type != "message" || x.Role != "user" || x.Status != "completed" {
		return "", false
	}
	for _, p := range x.Content {
		if p.Type == "input_audio" {
			return x.ID, true
		}
	}
	return "", false
}

// handleOOB consumes a server message that belongs to the out-of-band protocol.
// It queues the events to deliver in o.fifo and returns the client messages to
// send and whether the message was consumed. An unconsumed message is an
// ordinary server event for ParseServerEvent.
func (o *oobState) handleOOB(typ string, raw []byte) (sends [][]byte, consumed bool, err error) {
	switch typ {
	case "conversation.item.added", "conversation.item.done":
		if id, ok := callerAudioItem(raw); ok {
			b, err := o.request(id)
			if err != nil {
				return nil, false, err
			}
			if b != nil {
				sends = append(sends, b)
			}
		}
		return sends, false, nil
	case "response.created":
		consumed := o.correlate(raw)
		return o.takeCancels(), consumed, nil
	case "error":
		consumed := o.rejected(raw)
		return o.takeCancels(), consumed, nil
	case "response.done":
		consumed := o.complete(raw)
		return o.takeCancels(), consumed, nil
	case "response.output_text.delta", "response.output_text.done",
		"response.output_audio.delta", "response.output_audio_transcript.delta", "response.output_audio_transcript.done":
		var m struct {
			EventID    string `json:"event_id"`
			ResponseID string `json:"response_id"`
			ItemID     string `json:"item_id"`
			Delta      string `json:"delta"`
			Text       string `json:"text"`
			Transcript string `json:"transcript"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return nil, false, nil
		}
		if o.retired[m.ResponseID] {
			return nil, true, nil
		}
		req := o.byResponse[m.ResponseID]
		if req == nil {
			return nil, false, nil
		}
		o.textOrAudio(typ, req, m.EventID, m.Delta, m.Text)
		return o.takeCancels(), true, nil
	}
	return nil, false, nil
}

// takeCancels returns and clears the queued cancel messages.
func (o *oobState) takeCancels() [][]byte {
	out := o.cancel
	o.cancel = nil
	return out
}

// correlate handles response.created. The purpose echo and conversation_id
// decide what the response is. conversation_id is decoded as a raw value so a
// missing field is distinct from JSON null:
//
//   - echo and conversation_id null: the response was made with
//     conversation "none", as requested. It is bound to the oldest pending
//     request with no response id. With none pending, it is retired.
//   - echo and conversation_id non-null: the endpoint ignored conversation
//     "none". The request fails as conversation_not_none, the endpoint is
//     marked unsupported, and the response is retired.
//   - no echo and conversation_id missing: an ordinary response.
//
// A response retired here never reaches the call: its later events are dropped.
func (o *oobState) correlate(raw []byte) bool {
	var m struct {
		Response struct {
			ID             string          `json:"id"`
			ConversationID json.RawMessage `json:"conversation_id"`
			Metadata       map[string]any  `json:"metadata"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	id := m.Response.ID
	echoed := m.Response.Metadata != nil && m.Response.Metadata[oobPurposeKey] == oobPurpose
	convMissing := len(m.Response.ConversationID) == 0
	convNull := string(m.Response.ConversationID) == "null"
	// conversation_id set to a real id is the conversation's own response,
	// unless it echoes our purpose, which only an out-of-band request does.
	// A missing conversation_id is not evidence the endpoint ignored "none",
	// so it is treated like null, never like a conversation response.
	if !echoed && convMissing {
		return false
	}
	if !echoed && !convNull {
		return false
	}
	if o.retired[id] {
		return true
	}
	if !echoed {
		// conversation_id is null with no echo. Only an out-of-band request
		// can produce that, so it is bound to the oldest unbound request.
		// Without one, it is retired: its events must not reach the call.
		o.bindOldest(id)
		return true
	}
	if !convNull && !convMissing {
		// The endpoint did not honour conversation "none". Fail the item that
		// asked for it, latch the endpoint, and drop everything for this id.
		if req := o.oldestUnbound(); req != nil {
			req.responseID = id
			o.byResponse[id] = req
			ev := localFail(req.itemID, "conversation_not_none")
			ev.RawType = "response.created"
			o.fifo = append(o.fifo, ev)
			o.finish(req)
		}
		o.unsupported = true
		o.retire(id)
		return true
	}
	item, _ := m.Response.Metadata[oobItemKey].(string)
	for _, req := range o.pendingInOrder() {
		if req.itemID == item && req.responseID == "" {
			req.responseID = id
			o.byResponse[id] = req
			return true
		}
	}
	// The echo matches no pending request. It carries CallHarbor's purpose,
	// so it is retired and every event that follows is dropped.
	o.retire(id)
	return true
}

// pendingInOrder returns the pending requests, oldest first.
func (o *oobState) pendingInOrder() []*oobRequest {
	out := make([]*oobRequest, 0, len(o.pending))
	for _, req := range o.pending {
		out = append(out, req)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// oldestUnbound returns the oldest pending request with no response id yet.
func (o *oobState) oldestUnbound() *oobRequest {
	for _, req := range o.pendingInOrder() {
		if req.responseID == "" {
			return req
		}
	}
	return nil
}

// bindOldest binds a response id with no echo to the oldest unbound request,
// or retires it when none is waiting.
func (o *oobState) bindOldest(id string) {
	if req := o.oldestUnbound(); req != nil {
		req.responseID = id
		o.byResponse[id] = req
		return
	}
	o.retire(id)
}

// textOrAudio applies one event of a correlated out-of-band response. Audio
// and agent transcript events are dropped: the request asked for text only,
// so audio from the endpoint means it ignored the request, and the transcription
// is failed instead of being played into the call.
func (o *oobState) textOrAudio(typ string, req *oobRequest, eventID, delta, text string) {
	ev := realtime.Event{RawType: typ, EventID: eventID, ItemID: req.itemID, ResponseID: req.responseID, OutOfBand: true}
	switch typ {
	case "response.output_text.delta":
		ev.Kind, ev.Text = realtime.EventCallerTranscriptDelta, delta
		o.fifo = append(o.fifo, ev)
	case "response.output_text.done":
		// A response may carry several output_text.done parts. Each one is
		// delivered in order, and the recorder replaces the text with each
		// final, so only the last output_text.done is kept as the final text.
		ev.Kind, ev.Text = realtime.EventCallerTranscriptFinal, text
		req.final = true
		o.fifo = append(o.fifo, ev)
	case "response.output_audio.delta":
		// The endpoint ignored the text-only request. The item fails, the
		// endpoint is marked unsupported, and our own response is cancelled.
		// The cancel names this response id only, never the conversation.
		ev.Kind, ev.ErrCode = realtime.EventCallerTranscriptFailed, "modalities_ignored"
		o.fifo = append(o.fifo, ev)
		o.unsupported = true
		o.cancelSeq++
		o.cancel = append(o.cancel, cancelFor(fmt.Sprintf("%s%d", oobCancelPrefix, o.cancelSeq), req.responseID))
		o.finish(req)
	}
}

// cancelFor is the cancel for one out-of-band response. It names the response
// id and nothing else, so it can never cancel the call's conversation. It
// carries its own event id, so an error about it is recognised and dropped if
// the response had already finished.
func cancelFor(eventID, responseID string) []byte {
	b, _ := json.Marshal(map[string]string{"type": "response.cancel", "event_id": eventID, "response_id": responseID})
	return b
}

// complete handles response.done for a correlated out-of-band response. It is
// never delivered as the agent's response.done.
//
// A completed response with no text part and no output_text.done is a
// successful transcription with no speech: the prompt asks for no output when
// there is no speech, so it is an empty final, not a missing one. missing_text
// is reserved for a completed response whose output carries non-text content
// and no text part, where the text genuinely cannot be found.
func (o *oobState) complete(raw []byte) bool {
	var m struct {
		Response struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Output []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	if o.retired[m.Response.ID] {
		return true
	}
	req := o.byResponse[m.Response.ID]
	if req == nil {
		// Not correlated. It is still CallHarbor's response when it carries
		// the purpose, and its done must not surface as the agent's done.
		var meta struct {
			Response struct {
				Metadata map[string]any `json:"metadata"`
			} `json:"response"`
		}
		if json.Unmarshal(raw, &meta) == nil && meta.Response.Metadata[oobPurposeKey] == oobPurpose {
			o.retire(m.Response.ID)
			return true
		}
		return false
	}
	base := realtime.Event{RawType: "response.done", ItemID: req.itemID, ResponseID: req.responseID, OutOfBand: true}
	switch m.Response.Status {
	case "completed":
		if !req.final {
			text, ok := outputText(m.Response.Output)
			switch {
			case ok:
				base.Kind, base.Text = realtime.EventCallerTranscriptFinal, text
			case outputHasNonText(m.Response.Output):
				base.Kind, base.ErrCode = realtime.EventCallerTranscriptFailed, "missing_text"
			default:
				// Completed, no text and no other output: nothing was said.
				base.Kind, base.Text = realtime.EventCallerTranscriptFinal, ""
			}
			o.fifo = append(o.fifo, base)
		}
	case "cancelled", "failed", "incomplete":
		base.Kind, base.ErrCode = realtime.EventCallerTranscriptFailed, "response_"+m.Response.Status
		o.fifo = append(o.fifo, base)
	default:
		base.Kind, base.ErrCode = realtime.EventCallerTranscriptFailed, "response_"+providerCode(m.Response.Status)
		o.fifo = append(o.fifo, base)
	}
	o.finish(req)
	return true
}

// outputHasNonText reports whether the response output carries any content
// part other than a text part.
func outputHasNonText(out []struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}) bool {
	for _, item := range out {
		for _, p := range item.Content {
			if p.Type != "text" && p.Type != "output_text" {
				return true
			}
		}
	}
	return false
}

// outputText returns the text of the first text part of the response output.
// Only "text" and "output_text" parts count, never audio transcripts.
func outputText(out []struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}) (string, bool) {
	for _, item := range out {
		for _, p := range item.Content {
			if (p.Type == "text" || p.Type == "output_text") && p.Text != "" {
				return p.Text, true
			}
		}
	}
	return "", false
}

// rejected handles an error that belongs to an out-of-band request.
//
// An error whose error.event_id names a pending request is that request's
// rejection. An error with no event_id is attributed only in the narrow case
// that cannot be a call error by mistake: exactly one request is pending, it
// has no response id yet, and error.param is empty or names a response field.
// Any other error with no event_id is an ordinary call error and returns false.
//
// A rejection fails that item. The endpoint is marked unsupported only for an
// invalid_request_error; a rate limit or server error fails the item alone.
func (o *oobState) rejected(raw []byte) bool {
	var m struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Param   string `json:"param"`
			EventID string `json:"event_id"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	if strings.HasPrefix(m.Error.EventID, oobEventPrefix) {
		// An error about one of our own events is never a call error. A
		// cancel or request that already finished drops out here.
		req := o.pending[m.Error.EventID]
		if req == nil {
			return true
		}
		o.fifo = append(o.fifo, localFail(req.itemID, "out_of_band_rejected:"+providerCode(m.Error.Code)))
		o.finish(req)
		if m.Error.Type == "invalid_request_error" {
			o.unsupported = true
		}
		return true
	}
	if m.Error.EventID != "" || len(o.pending) != 1 {
		return false
	}
	// Heuristic attribution: no event id to match, so the item fails but the
	// endpoint is never latched on this path.
	for _, p := range o.pending {
		if p.responseID == "" && (m.Error.Param == "" || strings.HasPrefix(m.Error.Param, "response")) {
			o.fifo = append(o.fifo, localFail(p.itemID, "out_of_band_rejected:"+providerCode(m.Error.Code)))
			o.finish(p)
			return true
		}
	}
	return false
}
