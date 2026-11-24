package telnyx_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

func TestBridgeSinkConcurrentAgentAudioMarksAndInterrupts(t *testing.T) {
	b, err := voicebridge.New(g711, g711, time.Millisecond, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var sendMu sync.Mutex
	var sent int
	s, err := telnyx.NewBridgeSink(b, func(ctx context.Context, msg []byte) error {
		sendMu.Lock()
		sent++
		sendMu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 160)
	for i := range chunk {
		chunk[i] = audio.LinearToUlaw(int16(i * 50))
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				src := audio.SourceRef{ItemID: fmt.Sprintf("item-%d", i), ContentIndex: j}
				if err := s.AgentAudio(ctx, src, chunk); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Mark(fmt.Sprintf("ch-%d", j+1))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := s.Interrupt(ctx); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	sendMu.Lock()
	defer sendMu.Unlock()
	if sent == 0 {
		t.Fatal("no frames sent")
	}
}

func TestBridgeSinkInterruptCannotOvertakeInFlightAgentAudio(t *testing.T) {
	b, err := voicebridge.New(g711, g711, time.Millisecond, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	var calls int
	s, err := telnyx.NewBridgeSink(b, func(ctx context.Context, msg []byte) error {
		var m struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(msg, &m); err != nil {
			return err
		}
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(entered)
			<-release
		}
		mu.Lock()
		order = append(order, m.Event)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 160)
	for i := range chunk {
		chunk[i] = audio.LinearToUlaw(int16(i * 50))
	}
	ctx := context.Background()
	audioDone := make(chan error, 1)
	go func() {
		audioDone <- s.AgentAudio(ctx, audio.SourceRef{ItemID: "item-a"}, chunk)
	}()
	<-entered
	interruptDone := make(chan error, 1)
	go func() {
		interruptDone <- s.Interrupt(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case err := <-audioDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent audio never finished")
	}
	select {
	case err := <-interruptDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt never finished")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"media", "mark", "clear"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("wire order %v, want %v", order, want)
	}
}

func TestBridgeSinkDropsCutItemAudioAfterInterrupt(t *testing.T) {
	b, err := voicebridge.New(g711, g711, time.Millisecond, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var sent [][]byte
	s, err := telnyx.NewBridgeSink(b, func(ctx context.Context, msg []byte) error {
		sent = append(sent, msg)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 160)
	for i := range chunk {
		chunk[i] = audio.LinearToUlaw(int16(i * 50))
	}
	ctx := context.Background()
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "cut"}, chunk); err != nil {
		t.Fatal(err)
	}
	if err := s.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	before := len(sent)
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "cut", ContentIndex: 1}, chunk); err != nil {
		t.Fatal(err)
	}
	if len(sent) != before {
		t.Fatalf("cut item sent %d frames after interrupt", len(sent)-before)
	}
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "next"}, chunk); err != nil {
		t.Fatal(err)
	}
	if len(sent) == before {
		t.Fatal("next item was suppressed by an earlier cut")
	}
}
