package recording

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// WAVHeaderSize is the length of the canonical PCM WAV header synthesized at
// read time. It is never stored on disk.
const WAVHeaderSize = 44

// Channels is fixed: channel 0 is the remote party, channel 1 the agent.
const Channels = 2

// bytesPerFrame is one interleaved stereo frame of 16-bit PCM.
const bytesPerFrame = Channels * 2

// ErrInvalidWAVFormat reports a sample rate the header cannot describe.
var ErrInvalidWAVFormat = errors.New("recording: invalid WAV format")

// WAVHeader returns the 44-byte RIFF/WAVE header for 16-bit stereo PCM at
// sampleRate, describing pcmBytes of sample data. pcmBytes must be a whole
// number of frames.
func WAVHeader(sampleRate int, pcmBytes int64) ([]byte, error) {
	if sampleRate < 8000 || sampleRate > 48000 {
		return nil, fmt.Errorf("%w: sample rate %d", ErrInvalidWAVFormat, sampleRate)
	}
	if pcmBytes < 0 || pcmBytes%bytesPerFrame != 0 {
		return nil, fmt.Errorf("%w: pcm length %d is not whole frames", ErrInvalidWAVFormat, pcmBytes)
	}
	// RIFF size is the total file size minus the 8-byte RIFF preamble.
	riffSize := uint64(WAVHeaderSize-8) + uint64(pcmBytes)
	if riffSize > 0xFFFFFFFF {
		return nil, fmt.Errorf("%w: pcm length %d too large", ErrInvalidWAVFormat, pcmBytes)
	}
	h := make([]byte, WAVHeaderSize)
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(riffSize)) // #nosec G115 -- bounded above
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)
	binary.LittleEndian.PutUint16(h[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:24], Channels)
	binary.LittleEndian.PutUint32(h[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[28:32], uint32(sampleRate*bytesPerFrame))
	binary.LittleEndian.PutUint16(h[32:34], bytesPerFrame)
	binary.LittleEndian.PutUint16(h[34:36], 16)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(pcmBytes)) // #nosec G115 -- bounded by riffSize check
	return h, nil
}
