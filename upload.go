package main

// Talks to the Meeting Notes server: resumable upload of the recording (8 MB pieces,
// survives dropped connections), then the screenshots, then "finish" which queues it
// for transcription and minutes. Progress is saved in the recording folder, so
// `meetrec upload <folder>` can pick up where it stopped.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type client struct {
	cfg  config
	http *http.Client
}

func newClient(cfg config) *client {
	return &client{cfg: cfg, http: &http.Client{Timeout: 2 * time.Minute}}
}

func (c *client) do(method, path string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequest(method, c.cfg.Server+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	if c.cfg.CFClientID != "" { // optional Cloudflare Access service token
		req.Header.Set("CF-Access-Client-Id", c.cfg.CFClientID)
		req.Header.Set("CF-Access-Client-Secret", c.cfg.CFClientSecret)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Detail any `json:"detail"`
		}
		if json.Unmarshal(data, &e) == nil && e.Detail != nil {
			return fmt.Errorf("server said %d: %v", resp.StatusCode, e.Detail)
		}
		if bytes.Contains(data, []byte("cloudflareaccess")) || bytes.Contains(data, []byte("<html")) {
			return fmt.Errorf("server said %d: blocked before reaching the app (check the Cloudflare Access bypass for /api/device/)", resp.StatusCode)
		}
		return fmt.Errorf("server said %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *client) whoami() (string, error) {
	var me struct {
		Email string `json:"email"`
	}
	err := c.do("GET", "/api/device/me", nil, "", &me)
	return me.Email, err
}

// uploadState is kept in <recording>/upload.json
type uploadState struct {
	ID        string          `json:"id"`
	Shots     map[string]bool `json:"shots"`
	MeetingID string          `json:"meeting_id"`
}

type finishFields struct {
	Title         string `json:"title"`
	Language      string `json:"language"`
	NotesLanguage string `json:"notes_language"`
	Context       string `json:"context"`
	NumSpeakers   int    `json:"num_speakers"`
	ChannelLayout string `json:"channel_layout,omitempty"` // "mic,system" for two-channel recordings
}

var shotName = regexp.MustCompile(`^shot-\d+-t([\d.]+)\.jpg$`)

func (c *client) uploadRecording(dir string, fields finishFields, say func(string)) (string, error) {
	statePath := filepath.Join(dir, "upload.json")
	var st uploadState
	if b, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Shots == nil {
		st.Shots = map[string]bool{}
	}
	save := func() { b, _ := json.MarshalIndent(st, "", "  "); _ = os.WriteFile(statePath, b, 0o644) }
	if st.MeetingID != "" {
		return st.MeetingID, nil // already done
	}

	audio := filepath.Join(dir, "audio.wav")
	info, err := os.Stat(audio)
	if err != nil {
		return "", fmt.Errorf("no audio.wav in %s", dir)
	}
	size := info.Size()

	// 1. start (or resume) the upload
	received := int64(0)
	if st.ID != "" {
		var s struct {
			Received int64 `json:"received"`
		}
		if err := c.do("GET", "/api/device/uploads/"+st.ID, nil, "", &s); err == nil {
			received = s.Received
		} else {
			st.ID = ""
			st.Shots = map[string]bool{}
		}
	}
	if st.ID == "" {
		body, _ := json.Marshal(map[string]any{"filename": "audio.wav", "size": size, "content_type": "audio/wav"})
		var r struct {
			ID string `json:"id"`
		}
		if err := c.do("POST", "/api/device/uploads", bytes.NewReader(body), "application/json", &r); err != nil {
			return "", err
		}
		st.ID = r.ID
		save()
	}

	// 2. audio in pieces
	f, err := os.Open(audio)
	if err != nil {
		return "", err
	}
	defer f.Close()
	const chunk = 8 << 20
	buf := make([]byte, chunk)
	stalls := 0
	for received < size {
		n, err := f.ReadAt(buf, received)
		if err != nil && err != io.EOF {
			return "", err
		}
		var r struct {
			Received int64 `json:"received"`
		}
		before := received
		if err := c.do("PUT", fmt.Sprintf("/api/device/uploads/%s?offset=%d", st.ID, received), bytes.NewReader(buf[:n]), "application/octet-stream", &r); err != nil {
			say(fmt.Sprintf("upload problem (%v), retrying…", err))
			time.Sleep(time.Duration(1<<min(stalls, 4)) * time.Second)
			var s struct {
				Received int64 `json:"received"`
			}
			if c.do("GET", "/api/device/uploads/"+st.ID, nil, "", &s) == nil {
				r.Received = s.Received
			} else {
				r.Received = received
			}
		}
		received = max(received, r.Received)
		if received > before {
			stalls = 0
			say(fmt.Sprintf("uploading audio… %d%%", received*100/size))
		} else if stalls++; stalls > 8 {
			return "", fmt.Errorf("upload not making progress; run `meetrec upload %q` later to continue", dir)
		}
	}

	// 3. screenshots
	all, _ := filepath.Glob(filepath.Join(dir, "shots", "shot-*.jpg"))
	var files []string // previews; each one's .full.jpg is sent along with it
	for _, f := range all {
		if !strings.HasSuffix(f, ".full.jpg") {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	for i, p := range files {
		name := filepath.Base(p)
		m := shotName.FindStringSubmatch(name)
		if m == nil || st.Shots[name] {
			continue
		}
		t, _ := strconv.ParseFloat(m[1], 64)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		q := url.Values{"t": {strconv.FormatFloat(t, 'f', 1, 64)}}
		if err := c.do("PUT", "/api/device/uploads/"+st.ID+"/shots?"+q.Encode(), bytes.NewReader(data), "image/jpeg", nil); err != nil {
			return "", fmt.Errorf("screenshot %s: %w", name, err)
		}
		// the full-quality version, used in Outline / downloads for screenshots Gemini picks
		if full, err := os.ReadFile(strings.TrimSuffix(p, ".jpg") + ".full.jpg"); err == nil {
			q.Set("full", "1")
			if err := c.do("PUT", "/api/device/uploads/"+st.ID+"/shots?"+q.Encode(), bytes.NewReader(full), "image/jpeg", nil); err != nil {
				return "", fmt.Errorf("screenshot %s (full quality): %w", name, err)
			}
		}
		st.Shots[name] = true
		save()
		say(fmt.Sprintf("uploading screenshots… %d/%d", i+1, len(files)))
	}

	// 4. finish: queues transcription + minutes
	body, _ := json.Marshal(fields)
	var m struct {
		ID string `json:"id"`
	}
	if err := c.do("POST", "/api/device/uploads/"+st.ID+"/finish", bytes.NewReader(body), "application/json", &m); err != nil {
		return "", err
	}
	st.MeetingID = m.ID
	save()
	return m.ID, nil
}
