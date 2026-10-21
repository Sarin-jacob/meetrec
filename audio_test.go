package main

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// A 1 kHz tone at 48 kHz must come out as a 1 kHz tone at 16 kHz with the same level,
// and a 10 kHz tone (above the new 8 kHz limit) must be filtered out, not aliased.
func TestResampler(t *testing.T) {
	tone := func(freq float64) float64 {
		rs := newResampler(48000, 16000)
		var out []float32
		in := make([]float32, 480)
		for blk := 0; blk < 100; blk++ { // 1 s in 10 ms blocks, like the capture loop
			for i := range in {
				n := blk*480 + i
				in[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(n)/48000))
			}
			out = append(out, rs.process(in)...)
		}
		if len(out) < 15900 || len(out) > 16000 {
			t.Fatalf("got %d samples for 1 s", len(out))
		}
		var sum float64
		for _, v := range out[1000:] {
			sum += float64(v) * float64(v)
		}
		return math.Sqrt(sum / float64(len(out)-1000))
	}
	if rms := tone(1000); math.Abs(rms-0.3536) > 0.02 {
		t.Fatalf("1 kHz level changed: rms %.3f, want 0.354", rms)
	}
	if rms := tone(10000); rms > 0.02 {
		t.Fatalf("10 kHz not filtered: rms %.3f", rms)
	}
}

// Loopback sends nothing during silence: take() must pad, and a backlog must not grow.
func TestRingAlignment(t *testing.T) {
	r := &ring{}
	if got := r.take(1600, 4800); len(got) != 1600 || got[0] != 0 {
		t.Fatal("empty source should give silence")
	}
	r.write(make([]float32, 20000))
	r.take(1600, 4800)
	if len(r.buf) > 4800 {
		t.Fatalf("backlog not trimmed: %d", len(r.buf))
	}
}

// The WAV header must be valid even before close (crash safety).
func TestWavHeader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.wav")
	w, err := createWav(p, 1)
	if err != nil {
		t.Fatal(err)
	}
	w.write(make([]int16, 16000))
	w.close()
	b, _ := os.ReadFile(p)
	if string(b[:4]) != "RIFF" || string(b[36:40]) != "data" || len(b) != 44+32000 {
		t.Fatalf("bad wav: %d bytes", len(b))
	}
}

func TestStereoWav(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.wav")
	w, _ := createWav(p, 2)
	w.write(make([]int16, 2*16000)) // 1 s, two channels
	w.close()
	b, _ := os.ReadFile(p)
	if b[22] != 2 || len(b) != 44+64000 {
		t.Fatalf("bad stereo wav: ch=%d len=%d", b[22], len(b))
	}
}

// Devices running at high rates (96/192 kHz) with packets of any size: no crash, the right
// number of output samples, and a tone keeps its level. (A 192 kHz mic crashed v0.4.0.)
func TestResamplerHighRates(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, rate := range []int{22050, 44100, 48000, 88200, 96000, 176400, 192000} {
		r := newResampler(rate, TargetRate)
		seconds := 3
		in, out := 0, 0
		var outBuf []float32
		for in < rate*seconds {
			n := 1 + rng.Intn(2000) // packets from 1 sample to ~2000
			if rng.Intn(10) == 0 {
				n = 512
			}
			chunk := make([]float32, n)
			for i := range chunk {
				chunk[i] = float32(0.5 * math.Sin(2*math.Pi*1000*float64(in+i)/float64(rate)))
			}
			in += n
			o := r.process(chunk)
			out += len(o)
			outBuf = append(outBuf, o...)
		}
		want := in * TargetRate / rate
		if out < want-70 || out > want+2 { // the filter holds back a few samples
			t.Errorf("%d Hz: %d samples out for %d in, want about %d", rate, out, in, want)
		}
		var sum float64
		for _, v := range outBuf[TargetRate/10:] {
			sum += float64(v) * float64(v)
		}
		if rms := math.Sqrt(sum / float64(len(outBuf)-TargetRate/10)); rms < 0.33 || rms > 0.38 {
			t.Errorf("%d Hz: 1 kHz tone RMS %.3f, want about 0.354", rate, rms)
		}
	}
}
