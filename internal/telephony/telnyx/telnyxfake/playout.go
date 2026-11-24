// Package telnyxfake is an independent test emulator of the documented Telnyx
// bidirectional media playout queue. It is written from the Telnyx guide's
// prose, not from CallHarbor's adapter, and models exactly the documented
// behaviours:
//   - media frames are queued and played in submission order at real time;
//   - a mark is echoed when the media before it finishes;
//   - a mark submitted when nothing is queued is echoed immediately;
//   - clear drops queued media and echoes all queued marks.
//
// Played audio is recorded so tests can compare what CallHarbor believes was
// played against what the emulator actually played.
package telnyxfake

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

type entry struct {
	mark  string
	bytes []byte
}

// Echo is a mark returned to the client.
type Echo struct {
	Name string
	At   time.Time
}

// Playout emulates one bidirectional RTP stream at a fixed byte rate.
type Playout struct {
	bytesPerSec int
	now         time.Time
	queue       []entry
	headPlayed  int // bytes of queue[0] already played
	played      []byte
	echoes      []Echo
	// EchoMarksAfterClear can be disabled to emulate a lost echo.
	DropEchoes map[string]bool
}

// NewPlayout creates an emulator starting at t0.
func NewPlayout(bytesPerSec int, t0 time.Time) *Playout {
	return &Playout{bytesPerSec: bytesPerSec, now: t0, DropEchoes: map[string]bool{}}
}

// Receive handles one client→Telnyx text frame at the current time.
func (p *Playout) Receive(frame []byte) error {
	var m struct {
		Event string `json:"event"`
		Media *struct {
			Payload string `json:"payload"`
		} `json:"media"`
		Mark *struct {
			Name string `json:"name"`
		} `json:"mark"`
	}
	if err := json.Unmarshal(frame, &m); err != nil {
		return fmt.Errorf("100003 malformed_frame: %v", err)
	}
	switch m.Event {
	case "media":
		if m.Media == nil {
			return fmt.Errorf("100003 malformed_frame")
		}
		b, err := base64.StdEncoding.DecodeString(m.Media.Payload)
		if err != nil {
			return fmt.Errorf("100004 invalid_media")
		}
		p.queue = append(p.queue, entry{bytes: b})
	case "mark":
		if m.Mark == nil || m.Mark.Name == "" {
			return fmt.Errorf("100003 malformed_frame")
		}
		if p.audioQueued() == 0 {
			p.echo(m.Mark.Name)
		} else {
			p.queue = append(p.queue, entry{mark: m.Mark.Name})
		}
	case "clear":
		for _, e := range p.queue {
			if e.mark != "" {
				p.echo(e.mark)
			}
		}
		p.queue, p.headPlayed = nil, 0
	default:
		return fmt.Errorf("100003 malformed_frame: event %q", m.Event)
	}
	return nil
}

func (p *Playout) audioQueued() int {
	n := -p.headPlayed
	for _, e := range p.queue {
		n += len(e.bytes)
	}
	return n
}

func (p *Playout) echo(name string) {
	if !p.DropEchoes[name] {
		p.echoes = append(p.echoes, Echo{Name: name, At: p.now})
	}
}

// Advance plays queued audio for d and echoes marks reached.
func (p *Playout) Advance(d time.Duration) {
	budget := int(int64(d) * int64(p.bytesPerSec) / int64(time.Second))
	start := p.now
	consumed := 0
	for len(p.queue) > 0 {
		e := p.queue[0]
		if e.mark != "" {
			p.now = start.Add(time.Duration(int64(consumed) * int64(time.Second) / int64(p.bytesPerSec)))
			p.echo(e.mark)
			p.queue = p.queue[1:]
			continue
		}
		rem := len(e.bytes) - p.headPlayed
		if rem > budget {
			p.played = append(p.played, e.bytes[p.headPlayed:p.headPlayed+budget]...)
			p.headPlayed += budget
			break
		}
		p.played = append(p.played, e.bytes[p.headPlayed:]...)
		budget -= rem
		consumed += rem
		p.headPlayed = 0
		p.queue = p.queue[1:]
	}
	p.now = start.Add(d)
}

// Now returns emulator time.
func (p *Playout) Now() time.Time { return p.now }

// TakeEchoes returns and clears pending echoes.
func (p *Playout) TakeEchoes() []Echo {
	e := p.echoes
	p.echoes = nil
	return e
}

// Played returns all audio bytes actually played to the callee.
func (p *Playout) Played() []byte { return p.played }

// MarkFrame encodes an echo as Telnyx sends it.
func MarkFrame(streamID string, seq int, name string) []byte {
	b, _ := json.Marshal(map[string]any{
		"event": "mark", "stream_id": streamID, "sequence_number": fmt.Sprint(seq),
		"mark": map[string]string{"name": name},
	})
	return b
}
