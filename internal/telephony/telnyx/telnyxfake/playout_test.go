package telnyxfake

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

func media(b []byte) []byte {
	return []byte(`{"event":"media","media":{"payload":"` + base64.StdEncoding.EncodeToString(b) + `"}}`)
}

func mark(name string) []byte { return []byte(`{"event":"mark","mark":{"name":"` + name + `"}}`) }

func recv(t *testing.T, p *Playout, frames ...[]byte) {
	t.Helper()
	for _, f := range frames {
		if err := p.Receive(f); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlayoutMarksFollowRealTime(t *testing.T) {
	p := NewPlayout(8000, t0)
	recv(t, p, mark("empty"))
	if e := p.TakeEchoes(); len(e) != 1 || e[0].Name != "empty" || !e[0].At.Equal(t0) {
		t.Fatalf("mark on empty queue must echo immediately: %+v", e)
	}
	a, b := bytes.Repeat([]byte{1}, 160), bytes.Repeat([]byte{2}, 160)
	recv(t, p, media(a), mark("m1"), media(b), mark("m2"))
	p.Advance(30 * time.Millisecond)
	e := p.TakeEchoes()
	if len(e) != 1 || e[0].Name != "m1" || !e[0].At.Equal(t0.Add(20*time.Millisecond)) {
		t.Fatalf("%+v", e)
	}
	if !p.Now().Equal(t0.Add(30 * time.Millisecond)) {
		t.Fatalf("now %v", p.Now())
	}
	p.Advance(10 * time.Millisecond)
	if e := p.TakeEchoes(); len(e) != 1 || e[0].Name != "m2" {
		t.Fatalf("%+v", e)
	}
	if !bytes.Equal(p.Played(), append(append([]byte{}, a...), b...)) {
		t.Fatal("played audio mismatch")
	}
}

func TestPlayoutClearFlushesAndEchoes(t *testing.T) {
	p := NewPlayout(8000, t0)
	p.DropEchoes["lost"] = true
	recv(t, p, media(bytes.Repeat([]byte{1}, 160)), mark("m1"), mark("lost"))
	p.Advance(5 * time.Millisecond)
	recv(t, p, []byte(`{"event":"clear"}`))
	if e := p.TakeEchoes(); len(e) != 1 || e[0].Name != "m1" {
		t.Fatalf("%+v", e)
	}
	if len(p.Played()) != 40 {
		t.Fatalf("played %d bytes", len(p.Played()))
	}
	p.Advance(time.Second)
	if len(p.Played()) != 40 || len(p.TakeEchoes()) != 0 {
		t.Fatal("cleared audio played")
	}
}

func TestPlayoutRejectsMalformedFrames(t *testing.T) {
	p := NewPlayout(8000, t0)
	for _, f := range []string{
		`{`, `{"event":"media"}`, `{"event":"media","media":{"payload":"!!"}}`,
		`{"event":"mark"}`, `{"event":"mark","mark":{"name":""}}`, `{"event":"stop"}`,
	} {
		if err := p.Receive([]byte(f)); err == nil {
			t.Errorf("accepted %s", f)
		}
	}
}

func TestMarkFrame(t *testing.T) {
	var m struct {
		Event, StreamID, Seq string
		Mark                 struct{ Name string }
	}
	b := MarkFrame("s1", 7, "ch-1")
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if m.Event != "mark" || raw["stream_id"] != "s1" || raw["sequence_number"] != "7" || m.Mark.Name != "ch-1" {
		t.Fatalf("%s", b)
	}
}
