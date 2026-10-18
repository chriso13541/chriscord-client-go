package rnnoise

import (
	"math"
	"math/rand"
	"testing"
)

func energy(x []float32) float64 {
	s := 0.0
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return s / float64(len(x))
}

// Steady noise is taken out, and isn't mistaken for speech.
func TestNoiseRemoved(t *testing.T) {
	d := New()
	defer d.Close()
	r := rand.New(rand.NewSource(1))
	var before, after float64
	var vad float32
	for i := 0; i < 300; i++ { // 3 seconds
		f := make([]float32, FrameSize)
		for j := range f {
			f[j] = float32(r.NormFloat64() * 1000) // a loud fan / hiss
		}
		e := energy(f)
		v := d.Process(f)
		if i >= 100 { // after it has heard the noise for a second
			before += e
			after += energy(f)
			vad += v
		}
	}
	reduction := 10 * math.Log10(before/after)
	t.Logf("noise reduced by %.1f dB; mean speech probability %.3f", reduction, vad/200)
	if reduction < 10 {
		t.Fatalf("only %.1f dB less noise", reduction)
	}
	if vad/200 > 0.2 {
		t.Fatalf("noise taken for speech (%.2f)", vad/200)
	}
}

func BenchmarkFrame(b *testing.B) {
	d := New()
	defer d.Close()
	f := make([]float32, FrameSize)
	for i := 0; i < b.N; i++ {
		d.Process(f)
	}
}
