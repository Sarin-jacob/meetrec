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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
)

// Set by the release build (-X main.version=...); "dev" for local builds.
var version = "dev"

// ---------------------------------------------------------------- config

type config struct {
	Server         string   `json:"server"`
	Token          string   `json:"token"`
	CFClientID     string   `json:"cf_client_id,omitempty"`
	CFClientSecret string   `json:"cf_client_secret,omitempty"`
	GUI            guiPrefs `json:"gui"`
}

// What the floating window remembers between meetings.
type guiPrefs struct {
	Language string `json:"language,omitempty"` // last spoken language chosen
	People   int    `json:"people,omitempty"`   // 0 = not sure
	NoShots  bool   `json:"no_shots,omitempty"`
	NoMic    bool   `json:"no_mic,omitempty"`    // record without the microphone
	MicID    string `json:"mic_id,omitempty"`    // a chosen microphone ("" = Windows default)
	MicName  string `json:"mic_name,omitempty"`  // its name, for the menu when it's unplugged
	NoSystem bool   `json:"no_system,omitempty"` // no PC sound (an in-person meeting)
	App      string `json:"app,omitempty"`       // only this app's sound, by exe name ("" = all PC sound)
	AppPath  string `json:"app_path,omitempty"`  // where that app lives, for its icon
	X        int32  `json:"x,omitempty"`         // overlay position (0,0 = default corner)
	Y        int32  `json:"y,omitempty"`
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

var errNotSetUp = errors.New("not set up yet: run `meetrec login --server URL --token TOKEN` (get the token in the web app: Settings → Desktop recorder)")

func loadConfig() (config, error) {
	var c config
	b, err := os.ReadFile(configPath())
	if err != nil {
		return c, errNotSetUp
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Server == "" || c.Token == "" {
		return c, errNotSetUp
	}
	return c, nil
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
	cleanupOldExe()
	if len(os.Args) < 2 {
		if ownConsole() { // double-clicked: Explorer gave us a console of our own
			startGUI()
			return
		}
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "gui", "uninstall", "version", "--version", "help", "-h", "--help":
	default:
		if msg, err := install(); err != nil {
			fmt.Fprintln(os.Stderr, "note: couldn't install meetrec:", err)
		} else if msg != "" {
			fmt.Println(msg)
		}
	}
	var err error
	switch os.Args[1] {
	case "gui":
		startGUI()
	case "install":
		// done above
	case "uninstall":
		err = uninstall()
	case "login":
		err = cmdLogin(os.Args[2:])
	case "record":
		err = cmdRecord(os.Args[2:])
	case "upload":
		err = cmdUpload(os.Args[2:])
	case "sources":
		err = cmdSources()
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
  meetrec gui                                  the floating recorder (same as double-clicking
                                               meetrec.exe, or Start menu → meetrec)
  meetrec record [options]                     record in this terminal; press q to stop, then it uploads
  meetrec upload FOLDER                        upload (or finish uploading) a recording
  meetrec sources                              list microphones, and apps playing sound (for --mic / --app)
  meetrec displays                             list screens for --display
  meetrec windows                              list windows for --window
  meetrec uninstall                            remove meetrec from PATH and the Start menu

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
  --app NAME          record only this app's sound (e.g. zoom, ms-teams, chrome; see meetrec
                      sources), not every sound the PC makes. Needs Windows 11
  --mic NAME          use this microphone (part of its name; see meetrec sources)
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
	old, _ := loadConfig() // keep the floating window's preferences
	cfg := config{Server: strings.TrimRight(*server, "/"), Token: strings.TrimSpace(*token), CFClientID: *cfID, CFClientSecret: *cfSecret, GUI: old.GUI}
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
	app := fs.String("app", "", "")
	micName := fs.String("mic", "", "")
	noMic := fs.Bool("no-mic", false, "")
	noSystem := fs.Bool("no-system", false, "")
	micGain := fs.Float64("mic-gain", 1.0, "")
	sysGain := fs.Float64("system-gain", 1.0, "")
	noUpload := fs.Bool("no-upload", false, "")
	maxDur := fs.Duration("max", 0, "")
	mono := fs.Bool("mono", false, "")
	baseDir := fs.String("dir", defaultBaseDir(), "")
	fs.Usage = usage
	_ = fs.Parse(args)
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

	micID := ""
	if *micName != "" {
		mics, _, _ := listMics()
		for _, m := range mics {
			if strings.Contains(strings.ToLower(m.Name), strings.ToLower(*micName)) {
				micID = m.ID
				break
			}
		}
		if micID == "" {
			return fmt.Errorf("no microphone matching %q (see meetrec sources)", *micName)
		}
	}
	appExe := strings.ToLower(*app)
	if appExe != "" && !strings.HasSuffix(appExe, ".exe") {
		appExe += ".exe"
	}
	sess, err := startSession(recOptions{BaseDir: *baseDir, Title: *title, Shots: *shots != "off", Display: *display,
		Window: *window, NoMic: *noMic, NoSystem: *noSystem, MicDevice: micID, App: appExe, Mono: *mono,
		MicGain: *micGain, SysGain: *sysGain})
	if err != nil {
		return err
	}
	fields := finishFields{Title: *title, Language: *lang, NotesLanguage: *notesLang, Context: *context_, NumSpeakers: *people}
	if fields.Title == "" {
		fields.Title = "Online meeting " + sess.started.Format("2 Jan 15:04")
	}
	if err := sess.writeFields(fields); err != nil {
		return err
	}
	fields.ChannelLayout = sess.channelLayout

	fmt.Printf("Recording to %s\n", sess.dir)
	if sess.app.Exe != "" {
		fmt.Printf("PC sound: only %s\n", sess.app.Name)
	}
	if sess.shooter != nil {
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
loop:
	for {
		select {
		case <-sig:
			break loop
		case <-autoStop:
			sess.status("time limit reached")
			break loop
		case k := <-keys:
			switch k {
			case 'q', 'Q', 3, '\r', '\n':
				break loop
			case 'p', 'P':
				sess.togglePause()
			case 's', 'S':
				sess.screenshot()
			case 'x', 'X':
				if paused, ok := sess.toggleShots(); ok {
					sess.status(map[bool]string{true: "screenshots paused", false: "screenshots resumed"}[paused])
				}
			}
		case <-tick.C:
		}
		for _, m := range sess.messages() {
			fmt.Printf("\r\033[K  %s\n", m)
		}
		line := fmt.Sprintf("● %s  %s", fmtDur(sess.position()), map[bool]string{true: "PAUSED", false: "REC"}[sess.isPaused()])
		mic, pc := sess.levels()
		if mic >= 0 {
			line += "  mic " + meter(mic)
		}
		if pc >= 0 {
			line += "  pc " + meter(pc)
		}
		if sess.shooter != nil {
			line += fmt.Sprintf("  shots %d", sess.shotsTaken())
		}
		fmt.Printf("\r\033[K%s", line)
	}

	restoreKeys()
	signal.Stop(sig)
	fmt.Print("\r\033[K")
	dur, err := sess.stop()
	if err != nil {
		return err
	}
	fmt.Printf("Stopped: %s recorded, %d screenshot(s). Saved in %s\n", fmtDur(dur), sess.shotsTaken(), sess.dir)
	if dur < 3 {
		return errors.New("recording too short to send")
	}
	if *noUpload {
		fmt.Printf("Upload later with: meetrec upload %q\n", sess.dir)
		return nil
	}
	return upload(cfg, sess.dir, fields)
}

func cmdSources() error {
	mics, def, err := listMics()
	if err != nil {
		return err
	}
	fmt.Println("Microphones (--mic NAME):")
	if len(mics) == 0 {
		fmt.Println("  none found")
	}
	for _, m := range mics {
		mark := ""
		if m.Name == def {
			mark = "  (Windows default)"
		}
		fmt.Printf("  %s%s\n", m.Name, mark)
	}
	fmt.Println("\nApps with sound (--app NAME):")
	apps, err := listAudioApps()
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		fmt.Println("  none right now (an app shows up once it has played sound)")
	}
	for _, a := range apps {
		fmt.Printf("  %-10s %s\n", strings.TrimSuffix(a.Exe, ".exe"), a.Name)
	}
	if !appCaptureSupported() {
		fmt.Println("\nRecording a single app needs Windows 11 (build 20348+); this PC records all PC sound.")
	}
	return nil
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
