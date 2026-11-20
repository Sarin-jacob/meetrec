//go:build windows

package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestCompressAudioMakesASmallM4A(t *testing.T) {
	dir := t.TempDir()
	w, err := createWav(filepath.Join(dir, "audio.wav"), 2)
	if err != nil {
		t.Fatal(err)
	}
	// 20 s: a tone on the mic channel, another on the PC channel
	for s := 0; s < 20; s++ {
		pcm := make([]int16, 2*TargetRate)
		for i := 0; i < TargetRate; i++ {
			n := float64(s*TargetRate + i)
			pcm[2*i] = int16(8000 * math.Sin(2*math.Pi*440*n/TargetRate))
			pcm[2*i+1] = int16(8000 * math.Sin(2*math.Pi*1200*n/TargetRate))
		}
		w.write(pcm)
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	var last float64
	out, err := compressAudio(dir, func(p float64) { last = p })
	if err != nil {
		t.Fatal(err)
	}
	wi, _ := os.Stat(filepath.Join(dir, "audio.wav"))
	oi, _ := os.Stat(out)
	if filepath.Base(out) != "audio.m4a" || oi.Size()*4 > wi.Size() {
		t.Errorf("%s is %d bytes for %d of WAV", out, oi.Size(), wi.Size())
	}
	if err := checkM4A(out); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audio.part.m4a")); err == nil {
		t.Error("no half-made file left behind")
	}
	// a second call reuses it
	if last != 1 {
		t.Errorf("progress ended at %v", last)
	}
	again, err := compressAudio(dir, nil)
	if ai, _ := os.Stat(again); err != nil || !ai.ModTime().Equal(oi.ModTime()) {
		t.Errorf("made again: %v", err)
	}
}

func TestCompressAudioRefusesOtherFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "audio.wav"), []byte("not a wav file at all, just text, long enough for a header"), 0o644)
	if _, err := compressAudio(dir, nil); err == nil {
		t.Error("should refuse")
	}
	if _, err := os.Stat(filepath.Join(dir, "audio.m4a")); err == nil {
		t.Error("no .m4a for a bad file")
	}
}
