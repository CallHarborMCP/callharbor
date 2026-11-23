// Package audio defines wire audio formats, G.711 conversion, resampling,
// playout accounting and two-party timeline alignment shared by the telephony
// and voice adapters.
package audio

import (
	"fmt"
	"time"
)

// Encoding identifies a mono audio sample encoding carried on a media wire.
type Encoding string

const (
	PCMU Encoding = "PCMU"
	PCMA Encoding = "PCMA"
	// L16 is signed 16-bit linear PCM. Byte order is a property of each wire,
	// see Format.LittleEndian.
	L16 Encoding = "L16"
)

// Format is a fully specified mono wire format.
type Format struct {
	Encoding     Encoding
	SampleRate   int
	LittleEndian bool
}

func (f Format) String() string {
	return fmt.Sprintf("%s/%d", f.Encoding, f.SampleRate)
}

// BytesPerSample returns the encoded size of one mono sample.
func (f Format) BytesPerSample() int {
	if f.Encoding == L16 {
		return 2
	}
	return 1
}

// Validate rejects formats that cannot be timed exactly.
func (f Format) Validate() error {
	switch f.Encoding {
	case PCMU, PCMA:
		if f.SampleRate != 8000 {
			return fmt.Errorf("audio: %s requires 8000 Hz, got %d", f.Encoding, f.SampleRate)
		}
	case L16:
		switch f.SampleRate {
		case 8000, 16000, 24000, 48000:
		default:
			return fmt.Errorf("audio: unsupported L16 sample rate %d", f.SampleRate)
		}
	default:
		return fmt.Errorf("audio: unsupported encoding %q", f.Encoding)
	}
	return nil
}

// Duration converts an encoded byte count to media time. Partial samples are
// rejected because they indicate a framing defect.
func (f Format) Duration(n int) (time.Duration, error) {
	bps := f.BytesPerSample()
	if n%bps != 0 {
		return 0, fmt.Errorf("audio: %d bytes is not a whole number of %s samples", n, f)
	}
	samples := int64(n / bps)
	return time.Duration(samples) * time.Second / time.Duration(f.SampleRate), nil
}

// Samples returns the number of samples in d, truncated.
func (f Format) Samples(d time.Duration) int {
	return int(int64(d) * int64(f.SampleRate) / int64(time.Second))
}
