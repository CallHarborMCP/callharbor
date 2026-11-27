package recording

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestWAVHeaderCanonicalStereo16(t *testing.T) {
	h, err := WAVHeader(16000, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != WAVHeaderSize {
		t.Fatalf("header length = %d, want %d", len(h), WAVHeaderSize)
	}
	if string(h[0:4]) != "RIFF" || string(h[8:12]) != "WAVE" || string(h[12:16]) != "fmt " || string(h[36:40]) != "data" {
		t.Fatalf("chunk ids wrong: %q", h)
	}
	if got := binary.LittleEndian.Uint32(h[4:8]); got != 36+8 {
		t.Fatalf("RIFF size = %d, want 44", got)
	}
	if got := binary.LittleEndian.Uint16(h[22:24]); got != 2 {
		t.Fatalf("channels = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint32(h[24:28]); got != 16000 {
		t.Fatalf("sample rate = %d", got)
	}
	if got := binary.LittleEndian.Uint32(h[28:32]); got != 16000*4 {
		t.Fatalf("byte rate = %d", got)
	}
	if got := binary.LittleEndian.Uint16(h[32:34]); got != 4 {
		t.Fatalf("block align = %d", got)
	}
	if got := binary.LittleEndian.Uint16(h[34:36]); got != 16 {
		t.Fatalf("bits = %d", got)
	}
	if got := binary.LittleEndian.Uint32(h[40:44]); got != 8 {
		t.Fatalf("data size = %d", got)
	}
}

func TestWAVHeaderRejectsBadInput(t *testing.T) {
	if _, err := WAVHeader(4000, 0); !errors.Is(err, ErrInvalidWAVFormat) {
		t.Fatalf("low rate: %v", err)
	}
	if _, err := WAVHeader(16000, 3); !errors.Is(err, ErrInvalidWAVFormat) {
		t.Fatalf("partial frame: %v", err)
	}
	if _, err := WAVHeader(16000, -4); !errors.Is(err, ErrInvalidWAVFormat) {
		t.Fatalf("negative: %v", err)
	}
}

func TestWAVHeaderIsDeterministic(t *testing.T) {
	a, _ := WAVHeader(8000, 400)
	b, _ := WAVHeader(8000, 400)
	if !bytes.Equal(a, b) {
		t.Fatal("header must be deterministic")
	}
}
