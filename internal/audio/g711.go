package audio

import (
	"encoding/binary"
	"fmt"
)

// ITU-T G.711 companding, following the reference segment tables.

const (
	ulawBias = 0x84
	ulawClip = 32635
)

var alawSegEnd = [8]int{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}

// UlawToLinear decodes one μ-law byte.
func UlawToLinear(u byte) int16 {
	u = ^u
	exponent := (u >> 4) & 0x07
	mantissa := int(u & 0x0F)
	sample := ((mantissa << 3) + ulawBias) << exponent
	sample -= ulawBias
	if u&0x80 != 0 {
		return int16(-sample) // #nosec G115 -- G.711 μ-law magnitude ≤ 32124 fits int16
	}
	return int16(sample) // #nosec G115 -- G.711 μ-law magnitude ≤ 32124 fits int16
}

// LinearToUlaw encodes one 16-bit sample as μ-law.
func LinearToUlaw(s int16) byte {
	v := int(s)
	var sign byte
	if v < 0 {
		v = -v
		sign = 0x80
	}
	if v > ulawClip {
		v = ulawClip
	}
	v += ulawBias
	exponent := byte(7)
	for mask := 0x4000; v&mask == 0 && exponent > 0; mask >>= 1 {
		exponent--
	}
	mantissa := byte(v>>(exponent+3)) & 0x0F // #nosec G115 -- masked to 4 bits
	return ^(sign | exponent<<4 | mantissa)
}

// AlawToLinear decodes one A-law byte.
func AlawToLinear(a byte) int16 {
	a ^= 0x55
	t := int(a&0x0F) << 4
	seg := (a & 0x70) >> 4
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= seg - 1
	}
	if a&0x80 != 0 {
		return int16(t)
	}
	return int16(-t)
}

// LinearToAlaw encodes one 16-bit sample as A-law.
func LinearToAlaw(s int16) byte {
	pcm := int(s) >> 3
	mask := byte(0xD5)
	if pcm < 0 {
		mask = 0x55
		pcm = -pcm - 1
	}
	seg := 0
	for seg < len(alawSegEnd) && pcm > alawSegEnd[seg] {
		seg++
	}
	if seg >= len(alawSegEnd) {
		return 0x7F ^ mask
	}
	aval := byte(seg << 4)
	if seg < 2 {
		aval |= byte(pcm>>1) & 0x0F // #nosec G115 -- masked to 4 bits
	} else {
		aval |= byte(pcm>>seg) & 0x0F // #nosec G115 -- masked to 4 bits
	}
	return aval ^ mask
}

// Decode converts encoded wire bytes to linear samples, appending to dst.
func Decode(dst []int16, f Format, b []byte) ([]int16, error) {
	switch f.Encoding {
	case PCMU:
		for _, v := range b {
			dst = append(dst, UlawToLinear(v))
		}
	case PCMA:
		for _, v := range b {
			dst = append(dst, AlawToLinear(v))
		}
	case L16:
		if len(b)%2 != 0 {
			return dst, fmt.Errorf("audio: odd L16 byte count %d", len(b))
		}
		for i := 0; i < len(b); i += 2 {
			if f.LittleEndian {
				dst = append(dst, int16(binary.LittleEndian.Uint16(b[i:]))) // #nosec G115 -- two's-complement L16 reinterpretation
			} else {
				dst = append(dst, int16(binary.BigEndian.Uint16(b[i:]))) // #nosec G115 -- two's-complement L16 reinterpretation
			}
		}
	default:
		return dst, fmt.Errorf("audio: cannot decode %q", f.Encoding)
	}
	return dst, nil
}

// Encode converts linear samples to wire bytes, appending to dst.
func Encode(dst []byte, f Format, s []int16) ([]byte, error) {
	switch f.Encoding {
	case PCMU:
		for _, v := range s {
			dst = append(dst, LinearToUlaw(v))
		}
	case PCMA:
		for _, v := range s {
			dst = append(dst, LinearToAlaw(v))
		}
	case L16:
		for _, v := range s {
			if f.LittleEndian {
				dst = binary.LittleEndian.AppendUint16(dst, uint16(v)) // #nosec G115 -- two's-complement L16 reinterpretation
			} else {
				dst = binary.BigEndian.AppendUint16(dst, uint16(v)) // #nosec G115 -- two's-complement L16 reinterpretation
			}
		}
	default:
		return dst, fmt.Errorf("audio: cannot encode %q", f.Encoding)
	}
	return dst, nil
}
