# meetrec: record online meetings for Meeting Notes

`meetrec` records your microphone **and** whatever the PC is playing. That captures the other people in Zoom, Google Meet, Teams, Webex, Slack huddles, a phone-link call, or any other app; it doesn't care which meeting app you use. It can also take screenshots of shared slides. When you stop, it sends everything to your Meeting Notes server, which produces the transcript, speaker labels and minutes as usual. Screenshots are given to Gemini in the same single request (still one request per meeting, so the free tier is enough), and the most relevant ones appear inside the minutes.

It's a single 8 MB program. Double-click it for a small floating recorder button; there's also a terminal mode. While recording it uses next to no CPU and about 20 MB of RAM.

## Install

1. Download `meetrec.exe` from the [latest release](https://github.com/Sarin-jacob/meetrec/releases/latest) (Windows 10/11, 64-bit). It isn't code-signed, so Windows SmartScreen may say "Windows protected your PC": click **More info → Run anyway**.
2. **Double-click it once.** It installs itself in `%LOCALAPPDATA%\Programs\meetrec`, adds that folder to your PATH and adds **meetrec** to the Start menu (no admin rights needed). You can delete the downloaded file afterwards. Running a newer download later updates the installed copy the same way.
3. **Sign in, once:** in the web app open **Settings → Desktop recorder → Create token**, then paste the command it shows into a new terminal (PowerShell or Windows Terminal):
   ```
   meetrec login --server https://meetings.example.com --token mn_...
   ```
4. If the server is behind **Cloudflare Access**, the admin adds a **Bypass** policy for the path `/api/device/` (the token protects those endpoints). Alternatively, use a Cloudflare service token: `meetrec login ... --cf-id ID --cf-secret SECRET`.

`meetrec uninstall` removes it from PATH, the Start menu and its folder (recordings and the sign-in are kept).

## The floating recorder

Start **meetrec** from the Start menu (or double-click `meetrec.exe`). A small pill appears in the bottom-right corner; drag it anywhere. It shows the recording time and two level bars (your mic, the PC's sound), so you can see it's hearing the meeting.

**Click it** for the menu: Start / Stop, Pause, Screenshot now, the spoken **language** and the number of **people** (remembered for next time), where the sound comes from (below), slide screenshots on/off, the recordings folder, **Hide to taskbar** and Quit.

**Sound from:** *All PC sound* (the default: everything the PC plays), **only one app** (the apps that have played sound are listed, e.g. *Only Zoom*, *Only Teams*, *Only Chrome*), or *No PC sound* for an in-person meeting. Recording only the meeting app leaves out notification sounds, music and videos. The pill then shows that app's icon. If the app isn't running when you start, meetrec records all PC sound instead (so nothing is lost) and says so in the menu; if it closes mid-meeting, meetrec waits for it to come back. Recording one app needs Windows 11 (on Windows 10 those entries are greyed out).

**Microphone:** *Off*, the *Windows default* (follows it when you switch headsets), or a specific microphone. Both choices are remembered and are fixed while recording.

**Shortcuts** (while the pill is showing): **Ctrl+Alt+R** start / stop, **Ctrl+Alt+P** pause, **Ctrl+Alt+S** screenshot now, **Ctrl+Alt+X** pause screenshots.

**Hide to taskbar** swaps the pill for a taskbar button that shows the time; recording carries on, and the shortcuts are off so they can't be pressed by accident. Click the taskbar button to bring the pill back.

**When you stop**, a small window asks for the meeting title (taken from Teams/Webex window titles when it can), the language and the number of people. **Upload** sends it; **Discard** keeps the recording in its folder without sending it. If you don't touch it, it uploads with those defaults after a minute. The pill then shows the upload progress and "Sent ✓".

**Not uploaded (N)** appears in the menu when recordings are waiting to be sent: discarded ones, ones made while not signed in, or uploads that failed. Each recording (date, title, length) has **Upload** and **Open folder**, and **Upload all** sends them one after another (an interrupted upload continues where it stopped).

The pill hides itself from screen shares and recordings, so it never shows up in a presentation or in the screenshots.

## Recording in a terminal

```
meetrec record --title "Client call" --lang ml --people 4
● 00:12:31  REC  mic ▮▮▮▯▯▯  pc ▮▮▮▮▯▯  shots 4
Keys: q = stop   p = pause/resume   s = screenshot now   x = pause/resume screenshots
```

| Option | |
|---|---|
| `--lang` | spoken language: `hi`, `ml`, `ta`, `te`, `bn`, `mr`, `gu`, `kn`, `pa`… |
| `--people N` | number of people, if you know it (better speaker labels) |
| `--context "..."` | names and jargon, to help the minutes |
| `--shots off` | no screenshots |
| `--window W` | where to look for slides: `auto` (the meeting app's window, found automatically; the default), `screen`, or part of a window title (`meetrec windows` lists them) |
| `--display N` | the screen to use when there's no meeting window (`meetrec displays`) |
| `--app NAME` | only this app's sound, e.g. `zoom`, `ms-teams`, `chrome` (`meetrec sources` lists the apps that have played sound); needs Windows 11 |
| `--mic NAME` | this microphone (part of its name, see `meetrec sources`) instead of the Windows default |
| `--no-mic` / `--no-system` | only the PC's audio / only the mic |
| `--mono` | mix mic and PC into one channel (half the file size, but no automatic "you" label or echo removal) |
| `--max 2h` | stop automatically |
| `--no-upload` | keep it local; send later with `meetrec upload FOLDER` |

**Where recordings go:** `~/MeetRec/<date time title>/` (or the folder in the `MEETREC_DIR` environment variable) holds `audio.wav` (16 kHz stereo: left = your mic, right = the PC; about 230 MB per hour), `shots/` and `meeting.json`. The audio file stays playable even if the PC crashes mid-meeting. If the upload fails (no internet, server down), the recording stays there. `meetrec upload FOLDER` resumes where it stopped.

**You are labelled automatically.** Because your mic is on its own channel, the server knows which speech is yours and names that speaker with the name set in the web app (Settings → Your name). It also keeps learning your voice from these recordings, which helps phone recordings too.

**Headphones are optional.** Without them the mic also hears the speakers. The server finds that echo (how loud, how delayed) and removes it from the mic channel, keeping it wherever you talk over someone. Headphones still give the cleanest result.

## How the screenshots work

All of this runs on the laptop, on the CPU:

1. **Meeting window only.** The Zoom, Teams, Meet (browser tab), Webex… window is found automatically and captured on its own. Other apps, notifications and other monitors are left out, and it works even when the meeting window is behind other windows (not when it's minimised).
2. **Cutting out the video tiles.** The window is divided into a grid and each cell is sorted into one of three kinds:
   - **Backdrop:** the meeting app's flat dark stage. It's whatever flat dark shade runs along the window's edges, as in Zoom, Teams and Meet.
   - **Video:** a face's tile ([pigo](https://github.com/esimov/pigo), pure Go, MIT; it re-runs only when the layout changes or every 10 s), or camera-like pixels (many tones, unlike the flat background of a document).
   - **Candidate:** everything else.

   The shared content is the largest connected block of candidates. Toolbars, name labels and the title bar are separated from it by backdrop. Thin links, such as a floating toolbar button, are cut. A block that's mostly in constant motion is treated as video; a document being scrolled isn't.
3. **The right moment.** The recorder keeps the last settled frame before the content changes: a slide after its animations have built, or a whiteboard just before it's wiped. Near-duplicates are skipped, and long whiteboard sessions get a copy every few minutes.
4. **Two versions.** A small preview (1024 px) goes to Gemini with the minutes request. Gemini picks the useful ones and places them in the minutes. Outline and the downloads then get the **full-quality** version of the ones it picked.

Tested on a real one-minute Zoom recording. A Word document was shared and scrolled, with a video strip, name tiles, a view switch, an open menu and a switch to Google Sheets. Result: 2 distinct screenshots, both cropped to exactly the Word document, with no video, toolbars or backdrop.

Also tested on synthetic meetings (slides beside live video, a whiteboard drawn and wiped) and on Commons screenshots (Jitsi gallery/speaker: no shot; BigBlueButton whiteboard: cropped to the board).

Developer checks:
- `MEETREC_FRAMES=folder go test -run TestRealSequence -v`: frames named `f001-t000.0.png` (one every 3 s) are run in order, and the shots plus annotated frames are written to the folder.
- `MEETREC_IMAGES=folder go test -run TestRealImages -v`: single screenshots.

Known limits:
- A gallery of people with **cameras off** (static avatar tiles) can look like content. At most one such shot is taken, because nothing changes, and Gemini is told to skip it.
- Some apps can't draw a captured window, and minimised windows give nothing. Then the recorder falls back to the screen; use `--window screen` to always use the screen.

## What's possible and what isn't

**Works**
- **Any meeting app on Windows 10/11.** By default it records the PC's default output device ("loopback"), so it doesn't need to know about Zoom or Meet. It also follows the default device when you switch to headphones mid-meeting.
- **Just the meeting app (Windows 11).** Windows can capture one program and its helper processes on their own; meetrec uses that when you pick an app, so YouTube or a notification ping in the background stays out of the recording.
- **Your mic and everyone else in one file**, kept in sync on separate channels, at the 16 kHz the speech models use. You're labelled by name automatically and speaker echo is removed.
- **Slide and whiteboard screenshots** from the meeting window with video tiles cut out (see above). Press `s` to force one, or `x` to pause during anything private.
- **Screenshots inside the minutes.** Gemini reads them (slide titles, numbers on screen) and embeds the useful ones under the related point. They also show in the meeting page, the `.docx`/`.md` downloads and Outline.
- **Offline-tolerant.** Recording needs no network; uploads resume after drops.

**Not possible / not done**
- **Separate audio per remote person.** The PC only gets one mixed stream from the meeting app. Speaker labels come from voice fingerprints, as with phone recordings.
- **Audio from just the meeting app.** That needs Windows 11 process loopback, which is possible later. For now it records everything the PC plays, so mute other apps.
- **macOS.** macOS has no loopback for system audio without either a virtual audio driver (BlackHole) or ScreenCaptureKit, which needs Objective-C (cgo), a signed app and screen-recording permission. Not built.
- **Linux.** Possible: PulseAudio/PipeWire "monitor" sources are the same idea. Not built yet.
- **Chat messages, participant lists, the meeting's own captions.** Each meeting app has its own API and login (bot accounts). The goal was to stay app-agnostic and free, so meetrec only uses what's on your screen and in your speakers.
- **Recording without anyone knowing.** Don't: the Terms require everyone's consent. Most meeting apps also show their own "recording" notice only for their built-in recording, not for this.

**An alternative with no install at all:** Chrome/Edge can capture a tab's audio, or on Windows the whole system's audio, when you share your screen with "Share audio" ticked. A "Record online meeting" button in the web app could use that for Meet or Teams in the browser. The catch is that the tab must stay open and you have to pick the screen each time. It isn't built yet; meetrec covers desktop apps like Zoom, which the browser can't.

## Building

Plain Go, no C compiler needed:

```
go build -ldflags "-s -w" -o meetrec.exe .
go test ./...
```

The icon, the manifest (modern Windows controls, DPI awareness) and the version details live in `winres/` and are compiled into `rsrc_windows_amd64.syso`, which `go build` picks up. After changing them: `go run github.com/tc-hib/go-winres@v0.3.3 make --arch amd64`. Local builds report version `dev` and don't install themselves (set `MEETREC_INSTALL=1` to test that). `MEETREC_INSTANCE=test` runs a separate floating recorder next to the installed one (its own lock and window names), and `MEETREC_CONFIG` / `MEETREC_DIR` point it at a test sign-in and folder.

**Releases:** pushing a tag like `v0.2.0` runs `.github/workflows/release.yml`, which tests, builds `meetrec.exe` (with that version inside) and publishes it as a GitHub release. The web app's Settings page links to the latest one. A server that can't reach GitHub can serve its own copy instead: put `meetrec.exe` in `DATA_DIR/downloads/`.

## Files

| File | |
|---|---|
| `capture_windows.go` | WASAPI capture: microphone + loopback, follows the default device |
| `sources_windows.go` | microphones, apps playing sound, capturing a single app |
| `audio.go` | resampling to 16 kHz, keeping the two sources in sync, crash-safe WAV |
| `shots.go` | when to take a screenshot (last settled frame), preview + full quality |
| `vision.go` | finding the shared content: layout, faces (pigo), motion, photo-like areas |
| `window_windows.go` | capturing the meeting window |
| `upload.go` | resumable upload of audio and screenshots |
| `main.go` | commands, terminal keys, status line |
| `session.go` | one recording: capture threads, mixer, WAV file, screenshots |
| `gui_windows.go` | the floating recorder, taskbar button, shortcuts, the window after stopping |
| `install_windows.go` | install on first run, PATH, Start menu entry, uninstall |
| `winres/` | icon and Windows manifest |
