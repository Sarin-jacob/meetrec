package main

import (
	"math"
	"testing"
)

func TestUpsamplerKeepsSpeechBand(t *testing.T) {
	u := newUpsampler(3)
	// 1 kHz at 16 kHz, in pieces (the stream keeps its history between calls)
	var out []int16
	for piece := 0; piece < 10; piece++ {
		in := make([]float32, 1600)
		for i := range in {
			n := piece*1600 + i
			in[i] = 10000 * float32(math.Sin(2*math.Pi*1000*float64(n)/16000))
		}
		out = append(out, u.process(in)...)
	}
	if len(out) != 3*16000 {
		t.Fatalf("got %d samples, want %d", len(out), 3*16000)
	}
	// after the start-up, the 48 kHz signal is the same 1 kHz tone at the same level
	var peak float64
	for _, v := range out[3000:] {
		peak = math.Max(peak, math.Abs(float64(v)))
	}
	if peak < 9700 || peak > 10300 {
		t.Errorf("1 kHz level %v, want about 10000", peak)
	}
	// and it is a 1 kHz tone: compare with a fitted sine (some delay from the filter)
	best := 0.0
	for d := 0; d < 60; d++ {
		var dot, norm float64
		for i := 6000; i < 40000; i++ {
			ref := math.Sin(2 * math.Pi * 1000 * float64(i-d) / 48000)
			dot += float64(out[i]) * ref
			norm += ref * ref
		}
		best = math.Max(best, dot/norm)
	}
	if best < 9800 {
		t.Errorf("not a clean 1 kHz tone (fit %v)", best)
	}
}

func TestUpsamplerByOneJustConverts(t *testing.T) {
	out := newUpsampler(1).process([]float32{1.4, -40000, 40000})
	if len(out) != 3 || out[0] != 1 || out[1] != -32768 || out[2] != 32767 {
		t.Errorf("got %v", out)
	}
}
