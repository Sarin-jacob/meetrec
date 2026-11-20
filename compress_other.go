//go:build !windows

package main

import "errors"

// compressAudio: only Windows has the encoder meetrec uses; elsewhere the WAV is sent.
func compressAudio(dir string, progress func(float64)) (string, error) {
	return "", errors.New("compressing needs Windows")
}
