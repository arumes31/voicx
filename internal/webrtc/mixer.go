package webrtc

import "math"

// SoftClipMix sums equally-sized float32 PCM tracks into dst and applies a
// smooth tanh master limiter. Reusing dst makes the mixer allocation-free and
// keeps output in [-1, 1] even when many simultaneous sources peak together.
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
		dst[i] = float32(math.Tanh(float64(sample)))
	}
	return dst
}
