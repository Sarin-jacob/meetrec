package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestModeField(t *testing.T) {
	for in, want := range map[string]string{"": "", "work": "", "Work": "", "personal": "personal", " PERSONAL ": "personal"} {
		if got, err := modeField(in); err != nil || got != want {
			t.Errorf("modeField(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := modeField("private"); err == nil {
		t.Error("modeField(private) should fail")
	}
}

func TestFinishFieldsLeaveModeOutForWork(t *testing.T) {
	b, _ := json.Marshal(finishFields{Title: "Sync", Language: "hi"})
	if strings.Contains(string(b), "mode") {
		t.Errorf("work recordings must not send a mode (old servers): %s", b)
	}
	b, _ = json.Marshal(finishFields{Title: "Call", Language: "hi", Mode: "personal"})
	if !strings.Contains(string(b), `"mode":"personal"`) {
		t.Errorf("personal recordings send mode=personal: %s", b)
	}
}

// fakeServer answers the device upload API and records what "finish" was sent.
func fakeServer(t *testing.T, finishStatus int) (*httptest.Server, func() map[string]any) {
	var mu sync.Mutex
	var finished map[string]any
	var size int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/device/uploads":
			var s struct{ Size int64 }
			_ = json.Unmarshal(body, &s)
			size = s.Size
			_, _ = w.Write([]byte(`{"id":"u1","received":0}`))
		case r.Method == "PUT" && r.URL.Path == "/api/device/uploads/u1":
			_, _ = w.Write([]byte(`{"received":` + jsonInt(size) + `}`))
		case r.Method == "POST" && r.URL.Path == "/api/device/uploads/u1/finish":
			if finishStatus != 200 {
				w.WriteHeader(finishStatus)
				_, _ = w.Write([]byte(`{"detail":"Personal mode isn't turned on for you. Ask an admin."}`))
				return
			}
			mu.Lock()
			_ = json.Unmarshal(body, &finished)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"m1"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]any { mu.Lock(); defer mu.Unlock(); return finished }
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func recordingDir(t *testing.T, fields finishFields) string {
	dir := t.TempDir()
	w, err := createWav(filepath.Join(dir, "audio.wav"), 1)
	if err != nil {
		t.Fatal(err)
	}
	w.write(make([]int16, 16000))
	_ = w.close()
	b, _ := json.Marshal(fields)
	_ = os.WriteFile(filepath.Join(dir, "meeting.json"), b, 0o644)
	return dir
}

func TestUploadSendsTheMode(t *testing.T) {
	srv, finished := fakeServer(t, 200)
	c := newClient(config{Server: srv.URL, Token: "mn_x"})

	dir := recordingDir(t, finishFields{Title: "Clinic", Language: "hi", Mode: "personal"})
	fields, err := readFields(dir)
	if err != nil || fields.Mode != "personal" {
		t.Fatalf("meeting.json keeps the mode: %+v %v", fields, err)
	}
	if _, err := c.uploadRecording(dir, fields, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if finished()["mode"] != "personal" {
		t.Errorf("finish sent %v", finished())
	}

	srv2, finished2 := fakeServer(t, 200)
	dir = recordingDir(t, finishFields{Title: "Standup", Language: "hi"})
	fields, _ = readFields(dir)
	if _, err := newClient(config{Server: srv2.URL, Token: "mn_x"}).uploadRecording(dir, fields, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if _, ok := finished2()["mode"]; ok {
		t.Errorf("work upload sent a mode: %v", finished2())
	}
}

func TestRefusedPersonalUploadIsKept(t *testing.T) {
	srv, _ := fakeServer(t, 403)
	dir := recordingDir(t, finishFields{Title: "Clinic", Language: "hi", Mode: "personal"})
	fields, _ := readFields(dir)
	_, err := newClient(config{Server: srv.URL, Token: "mn_x"}).uploadRecording(dir, fields, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "Personal mode") {
		t.Fatalf("want the server's reason, got %v", err)
	}
	if uploaded(dir) {
		t.Error("a refused recording must still count as not uploaded")
	}
	if len(pendingRecordings(filepath.Dir(dir), "")) != 1 {
		t.Error("and be offered under Not uploaded")
	}
}
