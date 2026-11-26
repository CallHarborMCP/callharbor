package realtimews

import (
	"encoding/json"
	"fmt"
	"regexp"
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
	started    time.Time
	// final is set once the final text was emitted from output_text.done.
	final bool
}

// oobState is owned by one Session and touched only from Next, the single
// downlink reader, so it needs no lock.
type oobState struct {
	now         func() time.Time
	seq         int
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
	o.pending[eventID] = &oobRequest{itemID: itemID, eventID: eventID, started: o.now()}
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

// expire fails requests older than OutOfBandTimeout. It runs when Next
// processes a message, so no socket read is cancelled to implement a timer.
func (o *oobState) expire() {
	now := o.now()
	for _, req := range o.pending {
		if now.Sub(req.started) < OutOfBandTimeout {
			continue
		}
		o.finish(req)
		o.fifo = append(o.fifo, localFail(req.itemID, "timeout"))
	}
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
		return nil, o.correlate(raw), nil
	case "error":
		return nil, o.rejected(raw), nil
	case "response.done":
		return nil, o.complete(raw), nil
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
		return nil, true, nil
	}
	return nil, false, nil
}

// correlate maps a response.created that echoes this coordinator's metadata to
// its request. A response without the echo is an ordinary response.
func (o *oobState) correlate(raw []byte) bool {
	var m struct {
		Response struct {
			ID       string         `json:"id"`
			Metadata map[string]any `json:"metadata"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Response.Metadata == nil {
		return false
	}
	if m.Response.Metadata[oobPurposeKey] != oobPurpose {
		return false
	}
	item, _ := m.Response.Metadata[oobItemKey].(string)
	if o.retired[m.Response.ID] {
		return true
	}
	for _, req := range o.pending {
		if req.itemID == item && req.responseID == "" {
			req.responseID = m.Response.ID
			o.byResponse[req.responseID] = req
			return true
		}
	}
	// The echo matches no pending request. It carries CallHarbor's purpose,
	// so its events must never become call audio or agent text: retire it
	// and drop every event that follows.
	o.retire(m.Response.ID)
	return true
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
		ev.Kind, ev.Text = realtime.EventCallerTranscriptFinal, text
		req.final = true
		o.fifo = append(o.fifo, ev)
	case "response.output_audio.delta":
		ev.Kind, ev.ErrCode = realtime.EventCallerTranscriptFailed, "modalities_ignored"
		o.fifo = append(o.fifo, ev)
		o.finish(req)
	}
}

// complete handles response.done for a correlated out-of-band response. It is
// never delivered as the agent's response.done.
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
		// Not correlated (for example its response.created was never seen).
		// It is still CallHarbor's response when it carries the purpose, and
		// its done must not surface as the agent's response.done.
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
			// The final text is absent from output_text.done. Accept the
			// text from the response output only when it is a text part.
			text, ok := outputText(m.Response.Output)
			if !ok {
				base.Kind, base.ErrCode = realtime.EventCallerTranscriptFailed, "missing_text"
				o.fifo = append(o.fifo, base)
				o.finish(req)
				return true
			}
			base.Kind, base.Text = realtime.EventCallerTranscriptFinal, text
			o.fifo = append(o.fifo, base)
		}
	case "cancelled", "failed", "incomplete":
		base.Kind, base.ErrCode = realtime.EventCallerTranscriptFailed, "response_"+m.Response.Status
		o.fifo = append(o.fifo, base)
	default:
		base.Kind, base.ErrCode = realtime.EventCallerTranscriptFailed, "response_"+m.Response.Status
		o.fifo = append(o.fifo, base)
	}
	o.finish(req)
	return true
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

// rejected handles an error whose event_id names a pending out-of-band client
// event. Such an error is not a call error: it fails that transcription and
// marks the endpoint as not supporting out-of-band responses.
func (o *oobState) rejected(raw []byte) bool {
	var m struct {
		Error struct {
			Code    string `json:"code"`
			EventID string `json:"event_id"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	req := o.pending[m.Error.EventID]
	if req == nil {
		return false
	}
	code := m.Error.Code
	if code == "" {
		code = "unspecified"
	}
	ev := localFail(req.itemID, "out_of_band_rejected:"+code)
	o.fifo = append(o.fifo, ev)
	o.finish(req)
	o.unsupported = true
	return true
}
