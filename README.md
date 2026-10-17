# meetrec: record online meetings for Meeting Notes

`meetrec` records your microphone **and** whatever the PC is playing. That captures the other people in Zoom, Google Meet, Teams, Webex, Slack huddles, a phone-link call, or any other app; it doesn't care which meeting app you use. It can also take screenshots of shared slides. When you stop, it sends everything to your Meeting Notes server, which produces the transcript, speaker labels and minutes as usual. Screenshots are given to Gemini in the same single request (still one request per meeting, so the free tier is enough), and the most relevant ones appear inside the minutes.

It's a single 7 MB console program with no installer. While recording it uses next to no CPU and about 20 MB of RAM.

```
meetrec record --title "Weekly sync" --lang hi
● 00:12:31  REC  mic ▮▮▮▯▯▯  pc ▮▮▮▮▯▯  shots 4
Keys: q = stop   p = pause/resume   s = screenshot now   x = pause/resume screenshots
```

## Download

Get `meetrec.exe` from the [latest release](https://github.com/Sarin-jacob/meetrec/releases/latest) (Windows 10/11, 64-bit). It isn't code-signed, so the first time Windows SmartScreen may say "Windows protected your PC": click **More info → Run anyway**. Put it in a folder of its own, e.g. `C:\Tools\meetrec\`, and run it from a terminal (PowerShell or Windows Terminal) opened in that folder.

## Setup (once per PC)

1. In the web app, open **Settings → Desktop recorder → Create token**. Copy the command it shows and run it:
   ```
   meetrec login --server https://meetings.example.com --token mn_...
   ```
2. If the server is behind **Cloudflare Access**, the admin adds a **Bypass** policy for the path `/api/device/` (the token protects those endpoints). Alternatively, use a Cloudflare service token: `meetrec login ... --cf-id ID --cf-secret SECRET`.

## Recording

```
meetrec record --title "Client call" --lang ml --people 4
```

| Option | |
|---|---|
| `--lang` | spoken language: `hi`, `ml`, `ta`, `te`, `bn`, `mr`, `gu`, `kn`, `pa`… |
| `--people N` | number of people, if you know it (better speaker labels) |
| `--context "..."` | names and jargon, to help the minutes |
| `--shots off` | no screenshots |
| `--window W` | where to look for slides: `auto` (the meeting app's window, found automatically; the default), `screen`, or part of a window title (`meetrec windows` lists them) |
| `--display N` | the screen to use when there's no meeting window (`meetrec displays`) |
| `--no-mic` / `--no-system` | only the PC's audio / only the mic |
| `--mono` | mix mic and PC into one channel (half the file size, but no automatic "you" label or echo removal) |
| `--max 2h` | stop automatically |
| `--no-upload` | keep it local; send later with `meetrec upload FOLDER` |

**Where recordings go:** `~/MeetRec/<date time title>/` holds `audio.wav` (16 kHz stereo: left = your mic, right = the PC; about 230 MB per hour), `shots/` and `meeting.json`. The audio file stays playable even if the PC crashes mid-meeting. If the upload fails (no internet, server down), the recording stays there. `meetrec upload FOLDER` resumes where it stopped.

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
- **Any meeting app on Windows 10/11.** It records the PC's default output device ("loopback"), so it doesn't need to know about Zoom or Meet. It also follows the default device when you switch to headphones mid-meeting.
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

**Releases:** pushing a tag like `v0.2.0` runs `.github/workflows/release.yml`, which tests, builds `meetrec.exe` (with that version inside) and publishes it as a GitHub release. The web app's Settings page links to the latest one. A server that can't reach GitHub can serve its own copy instead: put `meetrec.exe` in `DATA_DIR/downloads/`.

## Files

| File | |
|---|---|
| `capture_windows.go` | WASAPI capture: microphone + loopback, follows the default device |
| `audio.go` | resampling to 16 kHz, keeping the two sources in sync, crash-safe WAV |
| `shots.go` | when to take a screenshot (last settled frame), preview + full quality |
| `vision.go` | finding the shared content: layout, faces (pigo), motion, photo-like areas |
| `window_windows.go` | capturing the meeting window |
| `upload.go` | resumable upload of audio and screenshots |
| `main.go` | commands, keys, status line |
