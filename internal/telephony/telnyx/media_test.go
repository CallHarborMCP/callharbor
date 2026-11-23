package telnyx_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/recording"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "media", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDocumentedMediaFramesParse(t *testing.T) {
	f, err := telnyx.ParseFrame(fixture(t, "connected.json"))
	if err != nil || f.Event != telnyx.FrameConnected {
		t.Fatalf("connected: %+v %v", f, err)
	}
	f, err = telnyx.ParseFrame(fixture(t, "start_pcmu.json"))
	if err != nil {
		t.Fatal(err)
	}
	if f.StreamID != "32DE0DEA-53CB-4B21-89A4-9E1819C043BC" || !f.HasSeq || f.Sequence != 1 ||
		f.Format != (audio.Format{Encoding: audio.PCMU, SampleRate: 8000}) || f.CallControlID == "" {
		t.Fatalf("start: %+v", f)
	}
	f, err = telnyx.ParseFrame(fixture(t, "stop.json"))
	if err != nil || f.Event != telnyx.FrameStop {
		t.Fatalf("stop: %+v %v", f, err)
	}
	f, err = telnyx.ParseFrame(fixture(t, "mark.json"))
	if err != nil || f.MarkName != "some_mark_name" {
		t.Fatalf("mark: %+v %v", f, err)
	}
	f, err = telnyx.ParseFrame(fixture(t, "dtmf.json"))
	if err != nil || f.Digit != "1" {
		t.Fatalf("dtmf: %+v %v", f, err)
	}
	f, err = telnyx.ParseFrame(fixture(t, "error.json"))
	if err != nil || f.ErrCode != 100003 {
		t.Fatalf("error: %+v %v", f, err)
	}
}

func mediaFrame(track string, seq, chunk int, ts uint32, payload []byte) []byte {
	b, _ := json.Marshal(map[string]any{
		"event": "media", "sequence_number": fmt.Sprint(seq), "stream_id": "s1",
		"media": map[string]any{"track": track, "chunk": fmt.Sprint(chunk), "timestamp": fmt.Sprint(ts), "payload": base64.StdEncoding.EncodeToString(payload)},
	})
	return b
}

func TestMediaFrameNumericFormsAndTracks(t *testing.T) {
	p := bytes.Repeat([]byte{0xFF}, 160)
	for _, raw := range [][]byte{
		mediaFrame("inbound", 4, 2, 320, p),
		[]byte(`{"event":"media","sequence_number":4,"stream_id":"s1","media":{"track":"inbound","chunk":2,"timestamp":320,"payload":"` + base64.StdEncoding.EncodeToString(p) + `"}}`),
	} {
		f, err := telnyx.ParseFrame(raw)
		if err != nil || f.Sequence != 4 || f.Chunk != 2 || f.Timestamp != 320 || len(f.Payload) != 160 {
			t.Fatalf("%s: %+v %v", raw, f, err)
		}
	}
}

func TestMediaFramesNegative(t *testing.T) {
	ok := base64.StdEncoding.EncodeToString([]byte{1, 2})
	cases := map[string]string{
		"not json":              `{"event":`,
		"unknown event":         `{"event":"pause"}`,
		"doc placeholder track": `{"event":"media","stream_id":"s","media":{"track":"inbound/outbound","timestamp":"5","payload":"` + ok + `"}}`,
		"request enum track":    `{"event":"media","stream_id":"s","media":{"track":"inbound_track","timestamp":"5","payload":"` + ok + `"}}`,
		"bad base64":            `{"event":"media","stream_id":"s","media":{"track":"inbound","timestamp":"5","payload":"***"}}`,
		"empty payload":         `{"event":"media","stream_id":"s","media":{"track":"inbound","timestamp":"5","payload":""}}`,
		"no timestamp":          `{"event":"media","stream_id":"s","media":{"track":"inbound","payload":"` + ok + `"}}`,
		"ts overflow":           `{"event":"media","stream_id":"s","media":{"track":"inbound","timestamp":"4294967296","payload":"` + ok + `"}}`,
		"non-numeric seq":       `{"event":"media","sequence_number":"x","stream_id":"s","media":{"track":"inbound","timestamp":"5","payload":"` + ok + `"}}`,
		"start no format":       `{"event":"start","stream_id":"s","start":{"call_control_id":"c"}}`,
		"start stereo":          `{"event":"start","stream_id":"s","start":{"call_control_id":"c","media_format":{"encoding":"PCMU","sample_rate":8000,"channels":2}}}`,
		"start opus":            `{"event":"start","stream_id":"s","start":{"call_control_id":"c","media_format":{"encoding":"OPUS","sample_rate":16000,"channels":1}}}`,
		"start pcmu 16k":        `{"event":"start","stream_id":"s","start":{"call_control_id":"c","media_format":{"encoding":"PCMU","sample_rate":16000,"channels":1}}}`,
		"start stream in body":  `{"event":"start","start":{"stream_id":"s","call_control_id":"c","media_format":{"encoding":"PCMU","sample_rate":8000,"channels":1}}}`,
		"mark no name":          `{"event":"mark","mark":{}}`,
		"dtmf multi":            `{"event":"dtmf","dtmf":{"digit":"12"}}`,
		"dtmf letter":           `{"event":"dtmf","dtmf":{"digit":"x"}}`,
		"error no code":         `{"event":"error","payload":{"title":"x"}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := telnyx.ParseFrame([]byte(raw)); !errors.Is(err, telnyx.ErrInvalidFrame) {
				t.Fatalf("got %v", err)
			}
		})
	}
	big := append([]byte(`{"event":"media","x":"`), bytes.Repeat([]byte("a"), telnyx.MaxMediaFrame)...)
	if _, err := telnyx.ParseFrame(big); !errors.Is(err, telnyx.ErrInvalidFrame) {
		t.Fatal("oversized frame accepted")
	}
}

func TestOutboundCommandShapes(t *testing.T) {
	m, _ := telnyx.MediaMessage([]byte{0xFF, 0x7F})
	if string(m) != `{"event":"media","media":{"payload":"/38="}}` {
		t.Fatalf("media %s", m)
	}
	k, _ := telnyx.MarkMessage("ch-1")
	if string(k) != `{"event":"mark","mark":{"name":"ch-1"}}` {
		t.Fatalf("mark %s", k)
	}
	if string(telnyx.ClearMessage()) != `{"event":"clear"}` {
		t.Fatal("clear shape")
	}
	if _, err := telnyx.MediaMessage(nil); err == nil {
		t.Fatal("empty media accepted")
	}
	if _, err := telnyx.MarkMessage(""); err == nil {
		t.Fatal("empty mark accepted")
	}
}

// tone returns PCMU bytes of a sine at hz for d at 8 kHz, so played audio is
// real decodable speech-band signal rather than opaque bytes.
func tone(hz float64, d time.Duration, phase int) []byte {
	n := int(d / (time.Second / 8000))
	out := make([]byte, n)
	for i := range out {
		v := 12000 * math.Sin(2*math.Pi*hz*float64(i+phase)/8000)
		out[i] = audio.LinearToUlaw(int16(v))
	}
	return out
}

type bridge struct {
	t      *testing.T
	fake   *telnyxfake.Playout
	ledger *audio.PlayoutLedger
}

func (b *bridge) send(src audio.SourceRef, payload []byte) {
	b.t.Helper()
	m, _ := telnyx.MediaMessage(payload)
	if err := b.fake.Receive(m); err != nil {
		b.t.Fatal(err)
	}
	name, err := b.ledger.Send(src, len(payload), b.fake.Now())
	if err != nil {
		b.t.Fatal(err)
	}
	k, _ := telnyx.MarkMessage(name)
	if err := b.fake.Receive(k); err != nil {
		b.t.Fatal(err)
	}
}

func (b *bridge) pump() []audio.MarkOutcome {
	b.t.Helper()
	var out []audio.MarkOutcome
	for i, e := range b.fake.TakeEchoes() {
		f, err := telnyx.ParseFrame(telnyxfake.MarkFrame("s1", 100+i, e.Name))
		if err != nil {
			b.t.Fatal(err)
		}
		o, err := b.ledger.Echo(f.MarkName, e.At)
		if err != nil {
			b.t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

func newBridge(t *testing.T) *bridge {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := audio.Format{Encoding: audio.PCMU, SampleRate: 8000}
	l, err := audio.NewPlayoutLedger(f, 5*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return &bridge{t: t, fake: telnyxfake.NewPlayout(8000, t0), ledger: l}
}

// Verifies the played-audio source of truth after barge-in: CallHarbor's
// ledger must agree with what the independent emulator actually played, and
// flushed marks must not be counted as playback.
func TestPlayedAudioAfterInterruption(t *testing.T) {
	b := newBridge(t)
	item1 := audio.SourceRef{ItemID: "item_A"}
	item2 := audio.SourceRef{ItemID: "item_B"}
	for i := 0; i < 10; i++ {
		b.send(item1, tone(440, 20*time.Millisecond, i*160))
	}
	b.fake.Advance(200 * time.Millisecond)
	for _, o := range b.pump() {
		if o != audio.MarkPlayed {
			t.Fatalf("complete item echo %v", o)
		}
	}
	for i := 0; i < 50; i++ {
		b.send(item2, tone(660, 20*time.Millisecond, i*160))
	}
	b.fake.Advance(330 * time.Millisecond) // caller barges in mid-chunk
	b.ledger.Clear(b.fake.Now())
	if err := b.fake.Receive(telnyx.ClearMessage()); err != nil {
		t.Fatal(err)
	}
	var played, flushed int
	for _, o := range b.pump() {
		switch o {
		case audio.MarkPlayed:
			played++
		case audio.MarkFlushed:
			flushed++
		default:
			t.Fatalf("unexpected outcome %v", o)
		}
	}
	if played != 16 || flushed != 34 {
		t.Fatalf("played=%d flushed=%d", played, flushed)
	}
	if b.ledger.Pending() != 0 {
		t.Fatalf("pending %d", b.ledger.Pending())
	}
	a, _ := b.ledger.Item(item1)
	if a.Interrupted || a.Confirmed != 200*time.Millisecond || a.Sent != a.Confirmed {
		t.Fatalf("item1 %+v", a)
	}
	c, _ := b.ledger.Item(item2)
	if !c.Interrupted || c.Sent != time.Second || c.Confirmed != 320*time.Millisecond || c.Estimated != 330*time.Millisecond || c.Discarded != 670*time.Millisecond {
		t.Fatalf("item2 %+v", c)
	}
	actual := len(b.fake.Played())
	want := int((200 + 330) * 8)
	if actual != want {
		t.Fatalf("emulator played %d bytes, want %d", actual, want)
	}
	pcmu := audio.Format{Encoding: audio.PCMU, SampleRate: 8000}
	if est := pcmu.Samples(a.Estimated + c.Estimated); est != actual {
		t.Fatalf("ledger estimate %d samples != actually played %d", est, actual)
	}
	e, err := recording.AgentEntry("Sure, I can book a table for two at seven tonight.", []string{"item_B"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if e.Fidelity != recording.FidelityInterrupted || !e.PlayedIsEstimate || e.Played != 330*time.Millisecond {
		t.Fatalf("transcript entry must flag interrupted text: %+v", e)
	}
}

// Telnyx echoes a mark immediately when nothing is queued. If CallHarbor
// believed audio was queued (e.g. Telnyx silently dropped media), the early
// echo must not be accepted as playback.
func TestEarlyEchoIsNotPlayback(t *testing.T) {
	b := newBridge(t)
	src := audio.SourceRef{ItemID: "item_C"}
	name, err := b.ledger.Send(src, 1600, b.fake.Now())
	if err != nil {
		t.Fatal(err)
	}
	k, _ := telnyx.MarkMessage(name) // media never reached the fake
	_ = b.fake.Receive(k)
	outs := b.pump()
	if len(outs) != 1 || outs[0] != audio.MarkEarly {
		t.Fatalf("got %v", outs)
	}
	it, _ := b.ledger.Item(src)
	if it.Confirmed != 0 || it.Unconfirmed != 200*time.Millisecond {
		t.Fatalf("%+v", it)
	}
	e, _ := recording.AgentEntry("Hello?", []string{"item_C"}, it)
	if e.Fidelity != recording.FidelityUnconfirmed || recording.Complete([]recording.Entry{e}) == nil {
		t.Fatalf("unconfirmed playout must fail completeness: %+v", e)
	}
}

func TestMissingEchoTimesOut(t *testing.T) {
	b := newBridge(t)
	src := audio.SourceRef{ItemID: "item_D"}
	b.send(src, tone(440, 100*time.Millisecond, 0))
	b.fake.DropEchoes["ch-1"] = true
	b.fake.Advance(100 * time.Millisecond)
	if len(b.pump()) != 0 {
		t.Fatal("dropped echo delivered")
	}
	if err := b.ledger.Expire(b.fake.Now().Add(time.Second)); err != nil {
		t.Fatalf("expired too early: %v", err)
	}
	if err := b.ledger.Expire(b.fake.Now().Add(3 * time.Second)); !errors.Is(err, audio.ErrPlayoutTimedOut) {
		t.Fatalf("got %v", err)
	}
	it, _ := b.ledger.Item(src)
	if it.Confirmed != 0 || it.Unconfirmed != 100*time.Millisecond {
		t.Fatalf("%+v", it)
	}
}

func TestEchoNegative(t *testing.T) {
	b := newBridge(t)
	if _, err := b.ledger.Echo("forged", b.fake.Now()); !errors.Is(err, audio.ErrUnknownMark) {
		t.Fatalf("got %v", err)
	}
	src := audio.SourceRef{ItemID: "x"}
	b.ledger.Send(src, 160, b.fake.Now())
	b.ledger.Send(src, 160, b.fake.Now())
	if _, err := b.ledger.Echo("ch-2", b.fake.Now().Add(time.Second)); !errors.Is(err, audio.ErrMarkOutOfOrder) {
		t.Fatalf("got %v", err)
	}
	if _, err := b.ledger.Send(src, 0, b.fake.Now()); err == nil {
		t.Fatal("empty chunk accepted")
	}
}
