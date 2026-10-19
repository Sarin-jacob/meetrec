package main

// Recordings that were never sent: discarded after recording, recorded while not signed in,
// or an upload that failed. The floating recorder lists them so they can be sent later.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type pendingRec struct {
	Dir     string
	Title   string
	Started time.Time
	Seconds float64
}

// pendingRecordings lists recordings in base that haven't been uploaded, newest first.
// skip is the folder being recorded right now.
func pendingRecordings(base, skip string) []pendingRec {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []pendingRec
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		if !e.IsDir() || (skip != "" && strings.EqualFold(filepath.Clean(dir), filepath.Clean(skip))) {
			continue
		}
		fields, err := readFields(dir)
		if err != nil {
			continue // not a meetrec recording
		}
		info, err := os.Stat(filepath.Join(dir, "audio.wav"))
		if err != nil {
			continue
		}
		if uploaded(dir) {
			continue
		}
		started := info.ModTime()
		if len(e.Name()) >= 15 {
			if t, err := time.ParseInLocation("2006-01-02 1504", e.Name()[:15], time.Local); err == nil {
				started = t
			}
		}
		out = append(out, pendingRec{Dir: dir, Title: fields.Title, Started: started, Seconds: wavSeconds(filepath.Join(dir, "audio.wav"))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

func uploaded(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "upload.json"))
	if err != nil {
		return false
	}
	var st uploadState
	return json.Unmarshal(b, &st) == nil && st.MeetingID != ""
}

// wavSeconds reads the length from the WAV header (0 if it can't).
func wavSeconds(path string) float64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var h [44]byte
	if _, err := f.Read(h[:]); err != nil || string(h[0:4]) != "RIFF" || string(h[8:12]) != "WAVE" {
		return 0
	}
	channels := int(binary.LittleEndian.Uint16(h[22:24]))
	rate := int(binary.LittleEndian.Uint32(h[24:28]))
	bits := int(binary.LittleEndian.Uint16(h[34:36]))
	info, err := f.Stat()
	if err != nil || channels == 0 || rate == 0 || bits == 0 {
		return 0
	}
	// the file size, not the header's data size: a recording cut off by a crash still counts
	return float64(info.Size()-44) / float64(rate*channels*bits/8)
}

// label for menus: "18 Oct 15:04 · Weekly sync · 42 min"
func (p pendingRec) label() string {
	mins := int(p.Seconds/60 + 0.5)
	var length string
	switch h, m := mins/60, mins%60; {
	case mins < 1:
		length = "under a minute"
	case h == 0:
		length = fmt.Sprintf("%d min", m)
	case m == 0:
		length = fmt.Sprintf("%d h", h)
	default:
		length = fmt.Sprintf("%d h %d min", h, m)
	}
	title := p.Title
	if strings.HasPrefix(title, "Online meeting ") {
		title = "" // the default title is just the date again
	}
	parts := []string{p.Started.Format("2 Jan 15:04")}
	if title != "" {
		parts = append(parts, shorten(title, 40))
	}
	return strings.Join(append(parts, length), " · ")
}
