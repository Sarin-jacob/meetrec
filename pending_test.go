package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPendingRecordings(t *testing.T) {
	base := t.TempDir()
	mk := func(name, title string, seconds int, meetingID string) string {
		dir := filepath.Join(base, name)
		_ = os.MkdirAll(dir, 0o755)
		w, err := createWav(filepath.Join(dir, "audio.wav"), 2)
		if err != nil {
			t.Fatal(err)
		}
		w.write(make([]int16, seconds*16000*2))
		_ = w.close()
		b, _ := json.Marshal(finishFields{Title: title, Language: "hi"})
		_ = os.WriteFile(filepath.Join(dir, "meeting.json"), b, 0o644)
		if meetingID != "" || meetingID == "partial" {
			b, _ := json.Marshal(uploadState{ID: "u1", MeetingID: meetingID})
			_ = os.WriteFile(filepath.Join(dir, "upload.json"), b, 0o644)
		}
		return dir
	}
	mk("2026-10-17 0900 Old", "Old sync", 125, "")
	mk("2026-10-18 1504 Weekly sync", "Weekly sync", 2*3600+65, "")
	mk("2026-10-18 1600 Sent", "Sent one", 30, "m123")
	recording := mk("2026-10-19 1000", "Online meeting 19 Oct 10:00", 5, "")
	_ = os.MkdirAll(filepath.Join(base, "not a recording"), 0o755)

	got := pendingRecordings(base, recording)
	if len(got) != 2 {
		t.Fatalf("want 2 pending (not the sent one, nor the one being recorded), got %+v", got)
	}
	if got[0].Title != "Weekly sync" || got[1].Title != "Old sync" {
		t.Errorf("newest first: %q, %q", got[0].Title, got[1].Title)
	}
	if got[0].Started.Format("2006-01-02 15:04") != "2026-10-18 15:04" {
		t.Errorf("start time from the folder name: %v", got[0].Started)
	}
	for want, p := range map[string]pendingRec{
		"18 Oct 15:04 · Weekly sync · 2 h 1 min": got[0],
		"17 Oct 09:00 · Old sync · 2 min":        got[1],
		"19 Oct 10:00 · under a minute": {Title: "Online meeting 19 Oct 10:00", Seconds: 5,
			Started: time.Date(2026, 10, 19, 10, 0, 0, 0, time.Local)},
	} {
		if p.label() != want {
			t.Errorf("label: got %q, want %q", p.label(), want)
		}
	}
	if s := wavSeconds(filepath.Join(base, "2026-10-17 0900 Old", "audio.wav")); s < 124.9 || s > 125.1 {
		t.Errorf("duration %v", s)
	}
}
