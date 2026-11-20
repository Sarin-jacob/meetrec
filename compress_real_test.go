//go:build windows

package main

import (
	"os"
	"testing"
	"time"
)

// MEETREC_COMPRESS=<folder with audio.wav>: compress a real recording and report.
func TestCompressReal(t *testing.T) {
	dir := os.Getenv("MEETREC_COMPRESS")
	if dir == "" {
		t.Skip("set MEETREC_COMPRESS")
	}
	start := time.Now()
	out, err := compressAudio(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(out)
	t.Logf("%s: %.1f MB in %s", out, float64(info.Size())/1e6, time.Since(start).Round(time.Millisecond))
}
