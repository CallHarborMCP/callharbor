package audio

import "fmt"

// Resampler is a streaming linear-interpolation sample-rate converter whose
// output position is derived from exact integer sample counts, so chunking
// never accumulates clock drift. It has no anti-alias filter; G.711 8 kHz
// passthrough is the preferred telephony path and avoids resampling entirely.
type Resampler struct {
	in, out  int64
	prev     int16
	havePrev bool
	inCount  int64
	outCount int64
}

// NewResampler converts from inRate to outRate samples per second.
func NewResampler(inRate, outRate int) (*Resampler, error) {
	if inRate <= 0 || outRate <= 0 {
		return nil, fmt.Errorf("audio: invalid resample rates %d->%d", inRate, outRate)
	}
	return &Resampler{in: int64(inRate), out: int64(outRate)}, nil
}

// Process appends the output samples that are fully determined by the input
// seen so far.
func (r *Resampler) Process(dst []int16, src []int16) []int16 {
	if len(src) == 0 {
		return dst
	}
	last := r.inCount + int64(len(src)) - 1
	at := func(i int64) int16 {
		if i < r.inCount {
			return r.prev
		}
		return src[i-r.inCount]
	}
	for {
		num := r.outCount * r.in
		idx := num / r.out
		frac := num % r.out
		need := idx
		if frac != 0 {
			need++
		}
		if need > last {
			break
		}
		var v int64
		if frac == 0 {
			v = int64(at(idx))
		} else {
			a, b := int64(at(idx)), int64(at(idx+1))
			v = a + (b-a)*frac/r.out
		}
		dst = append(dst, int16(v)) // #nosec G115 -- linear interpolation between two int16 samples stays in range
		r.outCount++
	}
	r.prev = src[len(src)-1]
	r.havePrev = true
	r.inCount += int64(len(src))
	return dst
}

// Flush emits remaining output positions up to the end of the input by
// holding the final sample.
func (r *Resampler) Flush(dst []int16) []int16 {
	if !r.havePrev {
		return dst
	}
	for {
		num := r.outCount * r.in
		if num >= r.inCount*r.out {
			return dst
		}
		dst = append(dst, r.prev)
		r.outCount++
	}
}

// Counts returns the total input consumed and output produced.
func (r *Resampler) Counts() (in, out int64) { return r.inCount, r.outCount }
