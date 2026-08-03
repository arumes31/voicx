package webrtc

import "math"

const softClipThreshold = 0.8

// SoftClipMix sums equally-sized float32 PCM tracks into dst and applies a
// a soft-knee limiter above the linear range. Reusing dst makes the mixer
// allocation-free and keeps output in [-1, 1] when many sources peak together.
func SoftClipMix(dst []float32, tracks ...[]float32) []float32 {
	for i := range dst {
		dst[i] = 0
	}
	for _, track := range tracks {
		limit := min(len(dst), len(track))
		for i := 0; i < limit; i++ {
			dst[i] += track[i]
		}
	}
	for i, sample := range dst {
		magnitude := math.Abs(float64(sample))
		if magnitude <= softClipThreshold {
			continue
		}
		limited := softClipThreshold + (1-softClipThreshold)*math.Tanh((magnitude-softClipThreshold)/(1-softClipThreshold))
		dst[i] = float32(math.Copysign(limited, float64(sample)))
	}
	return dst
}
