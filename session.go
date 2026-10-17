package main

// A recording session, shared by the terminal command (meetrec record) and the
// floating window (meetrec gui): capture threads, mixer, WAV file and screenshots.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

type recOptions struct {
	BaseDir          string // recordings go in BaseDir/<date time title>/
	Title            string // used in the folder name; may be empty
	Shots            bool
	Display          int
	Window           string
	NoMic, NoSystem  bool
	Mono             bool
	MicGain, SysGain float64
}

type session struct {
	dir           string
	started       time.Time
	channelLayout string
	mx            *mixer
	wav           *wavWriter
	shooter       *shotTaker
	stopCh        chan struct{}
	mixDone       chan struct{}
	cancel        context.CancelFunc
	wg            sync.WaitGroup

	mu       sync.Mutex
	msgs     []string
	paused   bool
	warned   bool
	stopped  bool
	duration float64
}

var unsafeName = regexp.MustCompile(`[\\/:*?"<>|]+`)

func defaultBaseDir() string {
	if d := os.Getenv("MEETREC_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "MeetRec")
}

func folderName(started time.Time, title string) string {
	name := started.Format("2006-01-02 1504")
	if title != "" {
		name += " " + unsafeName.ReplaceAllString(title, "-")
	}
	return name
}

func startSession(o recOptions) (*session, error) {
	if o.NoMic && o.NoSystem {
		return nil, errors.New("--no-mic and --no-system together leave nothing to record")
	}
	if o.MicGain == 0 {
		o.MicGain = 1
	}
	if o.SysGain == 0 {
		o.SysGain = 1
	}
	s := &session{started: time.Now()}
	s.dir = filepath.Join(o.BaseDir, folderName(s.started, o.Title))
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, err
	}
	stereo := !o.NoMic && !o.NoSystem && !o.Mono
	channels := 1
	if stereo {
		channels = 2
		s.channelLayout = "mic,system"
	}
	wav, err := createWav(filepath.Join(s.dir, "audio.wav"), channels)
	if err != nil {
		return nil, err
	}
	s.wav = wav

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mx = &mixer{micGain: float32(o.MicGain), sysGain: float32(o.SysGain), out: wav, stereo: stereo}
	if !o.NoMic {
		s.mx.mic = &ring{}
		s.wg.Add(1)
		go func() { defer s.wg.Done(); captureSource(ctx, srcMic, s.mx.mic, s.status) }()
	}
	if !o.NoSystem {
		s.mx.sys = &ring{}
		s.wg.Add(1)
		go func() { defer s.wg.Done(); captureSource(ctx, srcSystem, s.mx.sys, s.status) }()
	}
	s.stopCh = make(chan struct{})
	s.mixDone = make(chan struct{})
	go func() { s.mx.run(s.stopCh); close(s.mixDone) }()
	if o.Shots {
		s.shooter = newShotTaker(filepath.Join(s.dir, "shots"), o.Display, o.Window, s.mx.position, s.status)
		go s.shooter.run(s.stopCh)
	}
	return s, nil
}

// status queues a message from a capture thread for the UI to show.
func (s *session) status(m string) {
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
}

// messages returns and clears the queued messages, adding a warning if no PC audio
// has arrived after 20 s of a PC-audio-only recording.
func (s *session) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.warned && s.mx.position() > 20 && s.mx.mic == nil && s.mx.sys != nil && !s.mx.sys.active {
		s.warned = true
		s.msgs = append(s.msgs, "no PC audio yet: is the meeting audio playing on the default output device?")
	}
	out := s.msgs
	s.msgs = nil
	return out
}

func (s *session) position() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return s.duration
	}
	return s.mx.position()
}

func (s *session) isPaused() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.paused }

func (s *session) togglePause() bool {
	p := s.mx.togglePause()
	s.mu.Lock()
	s.paused = p
	s.mu.Unlock()
	return p
}

// levels: recent peak of mic and PC audio (-1 when that source isn't recorded).
func (s *session) levels() (mic, pc float32) {
	mic, pc = -1, -1
	if s.mx.mic != nil {
		mic = s.mx.mic.level()
	}
	if s.mx.sys != nil {
		pc = s.mx.sys.level()
	}
	return
}

func (s *session) screenshot() {
	if s.shooter != nil {
		s.shooter.takeNow()
	}
}

// toggleShots pauses/resumes screenshots; ok is false when screenshots are off.
func (s *session) toggleShots() (paused, ok bool) {
	if s.shooter == nil {
		return false, false
	}
	return s.shooter.togglePause(), true
}

func (s *session) shotsTaken() int {
	if s.shooter == nil {
		return 0
	}
	return s.shooter.taken()
}

// stop ends the recording and closes the WAV file. Safe to call twice.
func (s *session) stop() (float64, error) {
	s.mu.Lock()
	if s.stopped {
		d := s.duration
		s.mu.Unlock()
		return d, nil
	}
	s.mu.Unlock()
	close(s.stopCh)
	<-s.mixDone
	s.cancel()
	s.wg.Wait()
	err := s.wav.close()
	s.mu.Lock()
	s.stopped, s.duration = true, s.mx.position()
	s.mu.Unlock()
	return s.duration, err
}

// writeFields saves what the server needs to know about the recording (meeting.json).
func (s *session) writeFields(f finishFields) error {
	f.ChannelLayout = s.channelLayout
	b, _ := json.MarshalIndent(f, "", "  ")
	return os.WriteFile(filepath.Join(s.dir, "meeting.json"), b, 0o644)
}

// retitle renames the folder to include the title chosen after recording (best effort).
func (s *session) retitle(title string) {
	want := filepath.Join(filepath.Dir(s.dir), folderName(s.started, title))
	if want != s.dir {
		if err := os.Rename(s.dir, want); err == nil {
			s.dir = want
		}
	}
}
