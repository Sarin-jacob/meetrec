package main

// Platform-independent audio plumbing: resampling to 16 kHz, per-source buffers,
// a mixer that keeps mic and system audio aligned, and a crash-safe WAV writer.

import (
	"encoding/binary"
	"math"
	"os"
	"sync"
	"time"
)

// TargetRate is what the speech pipeline uses; recording at it keeps files small
// (16 kHz mono 16-bit = ~115 MB per hour).
const TargetRate = 16000

// ---------------------------------------------------------------- resampler

// resampler converts a stream from one rate to another: a windowed-sinc low-pass
// (below the new Nyquist, so no aliasing) followed by linear interpolation.
type resampler struct {
	step    float64 // input samples per output sample
	pos     float64 // position of the next output sample, relative to hist start
	taps    []float32
	hist    []float32 // filtered input not yet consumed
	firHist []float32 // last len(taps)-1 raw input samples
}

func newResampler(from, to int) *resampler {
	r := &resampler{step: float64(from) / float64(to)}
	if from == to {
		return r
	}
	cutoff := 0.45 * float64(to) / float64(from) // fraction of input rate
	n := 63
	r.taps = make([]float32, n)
	var sum float64
	for i := 0; i < n; i++ {
		x := float64(i - n/2)
		sinc := 2 * cutoff
		if x != 0 {
			sinc = math.Sin(2*math.Pi*cutoff*x) / (math.Pi * x)
		}
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1)) // Hann
		r.taps[i] = float32(sinc * w)
		sum += sinc * w
	}
	for i := range r.taps {
		r.taps[i] /= float32(sum)
	}
	r.firHist = make([]float32, n-1)
	return r
}

func (r *resampler) process(in []float32) []float32 {
	if r.taps == nil {
		return append([]float32(nil), in...)
	}
	// low-pass
	buf := append(r.firHist, in...)
	n := len(r.taps)
	for i := 0; i+n <= len(buf); i++ {
		var acc float32
		for j, t := range r.taps {
			acc += buf[i+j] * t
		}
		r.hist = append(r.hist, acc)
	}
	r.firHist = append(r.firHist[:0], buf[len(buf)-(n-1):]...)
	// interpolate
	var out []float32
	for r.pos+1 < float64(len(r.hist)) {
		i := int(r.pos)
		frac := float32(r.pos - float64(i))
		out = append(out, r.hist[i]*(1-frac)+r.hist[i+1]*frac)
		r.pos += r.step
	}
	// Drop what's been used. With a big rate ratio (e.g. a 96 or 192 kHz device) the next
	// output can lie beyond the samples we have: keep that remainder in pos, so those
	// samples are skipped as they arrive.
	drop := min(int(r.pos), len(r.hist))
	r.hist = append(r.hist[:0], r.hist[drop:]...)
	r.pos -= float64(drop)
	return out
}

// ---------------------------------------------------------------- ring buffer

type ring struct {
	mu     sync.Mutex
	buf    []float32
	peak   float32 // for the level meter
	active bool
}

func (r *ring) write(s []float32) {
	r.mu.Lock()
	r.buf = append(r.buf, s...)
	for _, v := range s {
		if a := float32(math.Abs(float64(v))); a > r.peak {
			r.peak = a
		}
	}
	r.active = true
	r.mu.Unlock()
}

// take returns exactly n samples, padding with silence when the source has fallen
// behind (WASAPI loopback sends nothing while nothing is playing). A backlog beyond
// maxLag is dropped so the sources can never drift apart.
func (r *ring) take(n, maxLag int) []float32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if extra := len(r.buf) - n - maxLag; extra > 0 {
		r.buf = r.buf[extra:]
	}
	out := make([]float32, n)
	k := copy(out, r.buf)
	r.buf = r.buf[k:]
	return out
}

func (r *ring) level() float32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.peak
	r.peak = 0
	return p
}

// ---------------------------------------------------------------- mixer

type mixer struct {
	mic, sys         *ring
	micGain, sysGain float32
	out              *wavWriter
	stereo           bool // write mic and PC audio as separate channels
	mu               sync.Mutex
	written          int64 // samples written to the file = position on the recording's timeline
	paused           bool
}

// run writes a block every 100 ms, lagging real time by 300 ms so normal device
// latency never turns into gaps.
func (m *mixer) run(stop <-chan struct{}) {
	const block = TargetRate / 10
	const lag = 3 * block
	start := time.Now()
	var due int64
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		target := int64(time.Since(start).Seconds()*TargetRate) - lag
		for due+block <= target {
			due += block
			var a, b []float32
			if m.mic != nil {
				a = m.mic.take(block, lag)
			}
			if m.sys != nil {
				b = m.sys.take(block, lag)
			}
			m.mu.Lock()
			paused := m.paused
			m.mu.Unlock()
			if paused {
				continue
			}
			var pcm []int16
			if a != nil && b != nil && m.stereo {
				// two channels: left = your mic, right = the PC's audio. The server uses
				// them to label you and to remove the echo of the speakers in your mic.
				pcm = make([]int16, 2*block)
				for i := 0; i < block; i++ {
					pcm[2*i] = int16(softClip(a[i]*m.micGain) * 32767)
					pcm[2*i+1] = int16(softClip(b[i]*m.sysGain) * 32767)
				}
			} else {
				pcm = make([]int16, block)
				for i := range pcm {
					var v float32
					if a != nil {
						v += a[i] * m.micGain
					}
					if b != nil {
						v += b[i] * m.sysGain
					}
					pcm[i] = int16(softClip(v) * 32767)
				}
			}
			m.out.write(pcm)
			m.mu.Lock()
			m.written += block
			m.mu.Unlock()
		}
	}
}

func (m *mixer) position() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return float64(m.written) / TargetRate
}

func (m *mixer) togglePause() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paused = !m.paused
	return m.paused
}

func softClip(v float32) float32 {
	if v > 1 || v < -1 {
		return float32(math.Tanh(float64(v)))
	}
	return v
}

// ---------------------------------------------------------------- WAV writer

// wavWriter writes 16 kHz 16-bit PCM (mono, or stereo = mic | PC audio) and rewrites the
// header every few seconds, so the file is playable even if the PC crashes mid-meeting.
type wavWriter struct {
	f         *os.File
	channels  int
	data      int64
	lastFixed time.Time
}

func createWav(path string, channels int) (*wavWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := &wavWriter{f: f, channels: channels}
	return w, w.header()
}

func (w *wavWriter) header() error {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+w.data))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], uint16(w.channels))
	binary.LittleEndian.PutUint32(h[24:], TargetRate)
	binary.LittleEndian.PutUint32(h[28:], uint32(TargetRate*2*w.channels))
	binary.LittleEndian.PutUint16(h[32:], uint16(2*w.channels))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(w.data))
	_, err := w.f.WriteAt(h, 0)
	return err
}

func (w *wavWriter) write(pcm []int16) {
	buf := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(v))
	}
	if _, err := w.f.WriteAt(buf, 44+w.data); err == nil {
		w.data += int64(len(buf))
	}
	if time.Since(w.lastFixed) > 5*time.Second {
		w.lastFixed = time.Now()
		_ = w.header()
		_ = w.f.Sync()
	}
}

func (w *wavWriter) close() error {
	if err := w.header(); err != nil {
		return err
	}
	return w.f.Close()
}
