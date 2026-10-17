// meetrec records online meetings (Zoom, Google Meet, Teams, Webex, any app) on a
// Windows PC: your microphone plus the PC's own audio output, with optional slide
// screenshots, then sends it to the Meeting Notes server for transcription, speaker
// labels and minutes.
//
//	meetrec login --server https://meetings.example.com --token mn_...
//	meetrec record --title "Weekly sync" --lang hi
//	meetrec upload "C:\Users\me\MeetRec\2026-10-17 1530 Weekly sync"
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Set by the release build (-X main.version=...); "dev" for local builds.
var version = "dev"

// ---------------------------------------------------------------- config

type config struct {
	Server         string `json:"server"`
	Token          string `json:"token"`
	CFClientID     string `json:"cf_client_id,omitempty"`
	CFClientSecret string `json:"cf_client_secret,omitempty"`
}

func configPath() string {
	if p := os.Getenv("MEETREC_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "meetrec", "config.json")
}

func loadConfig() (config, error) {
	var c config
	b, err := os.ReadFile(configPath())
	if err != nil {
		return c, errors.New("not set up yet: run `meetrec login --server URL --token TOKEN` (get the token in the web app: Settings → Desktop recorder)")
	}
	return c, json.Unmarshal(b, &c)
}

func saveConfig(c config) error {
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), b, 0o600)
}

// ---------------------------------------------------------------- main

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "login":
		err = cmdLogin(os.Args[2:])
	case "record":
		err = cmdRecord(os.Args[2:])
	case "upload":
		err = cmdUpload(os.Args[2:])
	case "displays":
		for i := 0; i < displayCount(); i++ {
			fmt.Println(displayInfo(i))
		}
	case "windows":
		if w, ok := findWindow("auto"); ok {
			fmt.Printf("Meeting window found automatically: %q\n\n", w.title)
		}
		for _, w := range listWindows() {
			fmt.Printf("%5dx%-5d %s\n", w.w, w.h, w.title)
		}
	case "version", "--version":
		fmt.Println("meetrec", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `meetrec: record online meetings (mic + PC audio + screenshots) for Meeting Notes

  meetrec login --server URL --token TOKEN     one-time setup (token: web app → Settings → Desktop recorder)
  meetrec record [options]                     record now; press q to stop, then it uploads
  meetrec upload FOLDER                        upload (or finish uploading) a recording
  meetrec displays                             list screens for --display
  meetrec windows                              list windows for --window

record options:
  --title TEXT        meeting title
  --lang CODE         spoken language: hi, ml, ta, te, bn, mr, gu, kn, pa, ... (default hi)
  --notes-lang NAME   language of the minutes (default English)
  --people N          number of people, if known (helps speaker labels)
  --context TEXT      names / jargon to help the minutes
  --shots auto|off    slide / whiteboard screenshots (default auto)
  --window W          where to look for slides: auto (the meeting app's window, found
                      automatically), screen, or part of a window title (meetrec windows)
  --display N         the screen to use when there's no meeting window (default 0)
  --no-mic            only the PC's audio (e.g. you're on headphones and muted)
  --no-system         only the microphone (an in-person meeting)
  --mic-gain X        mic volume factor (default 1.0)
  --system-gain X     PC audio volume factor (default 1.0)
  --max DURATION      stop automatically after this long, e.g. 90m or 2h
  --mono              mix mic and PC audio into one channel (half the size; no automatic
                      "you" label or echo removal)
  --no-upload         just record; upload later with meetrec upload
  --dir PATH          where recordings are kept (default ~/MeetRec)
`)
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", "", "server URL, e.g. https://meetings.example.com")
	token := fs.String("token", "", "token from Settings → Desktop recorder")
	cfID := fs.String("cf-id", "", "optional Cloudflare Access service token client id")
	cfSecret := fs.String("cf-secret", "", "optional Cloudflare Access service token secret")
	_ = fs.Parse(args)
	if *server == "" || *token == "" {
		return errors.New("both --server and --token are needed")
	}
	cfg := config{Server: strings.TrimRight(*server, "/"), Token: strings.TrimSpace(*token), CFClientID: *cfID, CFClientSecret: *cfSecret}
	email, err := newClient(cfg).whoami()
	if err != nil {
		return fmt.Errorf("couldn't sign in with that token: %w", err)
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("Signed in as %s. Settings saved to %s\n", email, configPath())
	return nil
}

func cmdUpload(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: meetrec upload FOLDER")
	}
	dir := args[0]
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fields, err := readFields(dir)
	if err != nil {
		return err
	}
	return upload(cfg, dir, fields)
}

func upload(cfg config, dir string, fields finishFields) error {
	say := func(s string) { fmt.Printf("\r\033[K%s", s) }
	id, err := newClient(cfg).uploadRecording(dir, fields, say)
	fmt.Println()
	if err != nil {
		return fmt.Errorf("%w\nThe recording is safe in %s; run `meetrec upload %q` to try again", err, dir, dir)
	}
	fmt.Printf("Sent. Transcript and minutes will appear at %s/#%s\n", cfg.Server, id)
	return nil
}

func readFields(dir string) (finishFields, error) {
	var f finishFields
	b, err := os.ReadFile(filepath.Join(dir, "meeting.json"))
	if err != nil {
		return f, fmt.Errorf("%s doesn't look like a meetrec recording (no meeting.json)", dir)
	}
	return f, json.Unmarshal(b, &f)
}

// ---------------------------------------------------------------- record

func cmdRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	title := fs.String("title", "", "")
	lang := fs.String("lang", "hi", "")
	notesLang := fs.String("notes-lang", "English", "")
	people := fs.Int("people", 0, "")
	context_ := fs.String("context", "", "")
	shots := fs.String("shots", "auto", "")
	display := fs.Int("display", 0, "")
	window := fs.String("window", "auto", "")
	noMic := fs.Bool("no-mic", false, "")
	noSystem := fs.Bool("no-system", false, "")
	micGain := fs.Float64("mic-gain", 1.0, "")
	sysGain := fs.Float64("system-gain", 1.0, "")
	noUpload := fs.Bool("no-upload", false, "")
	maxDur := fs.Duration("max", 0, "")
	mono := fs.Bool("mono", false, "")
	home, _ := os.UserHomeDir()
	baseDir := fs.String("dir", filepath.Join(home, "MeetRec"), "")
	fs.Usage = usage
	_ = fs.Parse(args)
	if *noMic && *noSystem {
		return errors.New("--no-mic and --no-system together leave nothing to record")
	}
	if *display < 0 || *display >= displayCount() {
		return fmt.Errorf("no display %d (see meetrec displays)", *display)
	}

	var cfg config
	if !*noUpload {
		var err error
		if cfg, err = loadConfig(); err != nil {
			return err
		}
	}

	started := time.Now()
	name := started.Format("2006-01-02 1504")
	if *title != "" {
		name += " " + regexp.MustCompile(`[\\/:*?"<>|]+`).ReplaceAllString(*title, "-")
	}
	dir := filepath.Join(*baseDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fields := finishFields{Title: *title, Language: *lang, NotesLanguage: *notesLang, Context: *context_, NumSpeakers: *people}
	if fields.Title == "" {
		fields.Title = "Online meeting " + started.Format("2 Jan 15:04")
	}
	b, _ := json.MarshalIndent(fields, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meeting.json"), b, 0o644); err != nil {
		return err
	}

	stereo := !*noMic && !*noSystem && !*mono
	channels := 1
	if stereo {
		channels = 2
		fields.ChannelLayout = "mic,system"
	}
	b, _ = json.MarshalIndent(fields, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meeting.json"), b, 0o644); err != nil {
		return err
	}
	wav, err := createWav(filepath.Join(dir, "audio.wav"), channels)
	if err != nil {
		return err
	}

	// status messages from the capture threads show up above the status line
	var msgMu sync.Mutex
	var msgs []string
	status := func(s string) { msgMu.Lock(); msgs = append(msgs, s); msgMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	mx := &mixer{micGain: float32(*micGain), sysGain: float32(*sysGain), out: wav, stereo: stereo}
	var wg sync.WaitGroup
	if !*noMic {
		mx.mic = &ring{}
		wg.Add(1)
		go func() { defer wg.Done(); captureSource(ctx, srcMic, mx.mic, status) }()
	}
	if !*noSystem {
		mx.sys = &ring{}
		wg.Add(1)
		go func() { defer wg.Done(); captureSource(ctx, srcSystem, mx.sys, status) }()
	}
	stop := make(chan struct{})
	mixDone := make(chan struct{})
	go func() { mx.run(stop); close(mixDone) }()

	var shooter *shotTaker
	if *shots != "off" {
		shooter = newShotTaker(filepath.Join(dir, "shots"), *display, *window, mx.position, status)
		go shooter.run(stop)
	}

	fmt.Printf("Recording to %s\n", dir)
	if shooter != nil {
		fmt.Println("Screenshots: shared slides / whiteboards in the meeting window, video tiles cropped out. Press x to pause.")
	}
	fmt.Println("Keys: q = stop   p = pause/resume   s = screenshot now   x = pause/resume screenshots")
	if !*noMic && !*noSystem && *mono {
		fmt.Println("Tip: use headphones, otherwise the mic also picks up the speakers and voices are doubled.")
	}

	keys := readKeys()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	tick := time.NewTicker(500 * time.Millisecond)
	var autoStop <-chan time.Time
	if *maxDur > 0 {
		autoStop = time.After(*maxDur)
	}
	paused := false
	silentWarned := false
loop:
	for {
		select {
		case <-sig:
			break loop
		case <-autoStop:
			status("time limit reached")
			break loop
		case k := <-keys:
			switch k {
			case 'q', 'Q', 3, '\r', '\n':
				break loop
			case 'p', 'P':
				paused = mx.togglePause()
			case 's', 'S':
				if shooter != nil {
					shooter.takeNow()
				}
			case 'x', 'X':
				if shooter != nil {
					if shooter.togglePause() {
						status("screenshots paused")
					} else {
						status("screenshots resumed")
					}
				}
			}
		case <-tick.C:
		}
		msgMu.Lock()
		for _, m := range msgs {
			fmt.Printf("\r\033[K  %s\n", m)
		}
		msgs = msgs[:0]
		msgMu.Unlock()
		line := fmt.Sprintf("● %s  %s", fmtDur(mx.position()), map[bool]string{true: "PAUSED", false: "REC"}[paused])
		if mx.mic != nil {
			line += "  mic " + meter(mx.mic.level())
		}
		if mx.sys != nil {
			line += "  pc " + meter(mx.sys.level())
		}
		if shooter != nil {
			line += fmt.Sprintf("  shots %d", shooter.taken())
		}
		fmt.Printf("\r\033[K%s", line)
		if !silentWarned && mx.position() > 20 && mx.mic == nil && mx.sys != nil && !mx.sys.active {
			silentWarned = true
			status("no PC audio yet: is the meeting audio playing on the default output device?")
		}
	}

	restoreKeys()
	signal.Stop(sig)
	fmt.Print("\r\033[K")
	close(stop)
	<-mixDone
	cancel()
	wg.Wait()
	if err := wav.close(); err != nil {
		return err
	}
	dur := mx.position()
	n := 0
	if shooter != nil {
		n = shooter.taken()
	}
	fmt.Printf("Stopped: %s recorded, %d screenshot(s). Saved in %s\n", fmtDur(dur), n, dir)
	if dur < 3 {
		return errors.New("recording too short to send")
	}
	if *noUpload {
		fmt.Printf("Upload later with: meetrec upload %q\n", dir)
		return nil
	}
	return upload(cfg, dir, fields)
}

func fmtDur(sec float64) string {
	s := int(sec)
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
}

func meter(peak float32) string {
	n := int(peak * 12)
	if n > 6 {
		n = 6
	}
	return strings.Repeat("▮", n) + strings.Repeat("▯", 6-n)
}

// ---------------------------------------------------------------- keyboard

var oldState *term.State

func readKeys() <-chan byte {
	ch := make(chan byte, 8)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		if st, err := term.MakeRaw(fd); err == nil {
			oldState = st
		}
	}
	go func() {
		buf := make([]byte, 1)
		for {
			if n, err := os.Stdin.Read(buf); err != nil || n == 0 {
				return
			}
			ch <- buf[0]
		}
	}()
	return ch
}

func restoreKeys() {
	if oldState != nil {
		_ = term.Restore(int(os.Stdin.Fd()), oldState)
	}
}
