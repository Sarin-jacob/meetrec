package main

// Slide / whiteboard screenshots.
//
// Every few seconds the meeting window (or a screen) is captured and the shared content
// is located (vision.go): video tiles and moving areas are cut away. What gets saved is
// the LAST settled frame before the content changes: a slide after its animations have
// built, a whiteboard just before it's wiped, a document before scrolling on. Each shot
// is saved twice: a small preview (sent to Gemini to choose from) and a full-quality
// crop (used in Outline and downloads for the ones Gemini picks).

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kbinani/screenshot"
	"golang.org/x/image/draw"
)

type shotTaker struct {
	dir      string
	display  int
	window   string // "auto", "screen", or part of a window title
	every    time.Duration
	max      int
	timeline func() float64 // position in the recording, seconds
	status   func(string)

	mu     sync.Mutex
	count  int
	paused bool
	force  chan struct{}
	where  string // what's being captured, for the status line

	an     *analyzer
	hwnd   uintptr
	hwndAt time.Time
	cur    *content    // what's on screen now
	saved  [][]float32 // thumbnails of every shot kept, to skip repeats
}

// content being shown: its location, latest frame, and latest *settled* frame.
type content struct {
	rect       image.Rectangle
	thumb      []float32
	settled    *image.RGBA
	settledAt  float64 // timeline position of the settled frame
	latest     *image.RGBA
	latestAt   float64
	frames     int
	firstThumb []float32
	since      time.Time
}

const thumbW, thumbH = 48, 27

func newShotTaker(dir string, display int, window string, timeline func() float64, status func(string)) *shotTaker {
	return &shotTaker{dir: dir, display: display, window: window, every: 3 * time.Second, max: 120,
		timeline: timeline, status: status, force: make(chan struct{}, 1), an: newAnalyzer()}
}

func (s *shotTaker) run(stop <-chan struct{}) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		s.status("screenshots: " + err.Error())
		return
	}
	tick := time.NewTicker(s.every)
	defer tick.Stop()
	lastErr := ""
	for {
		forced := false
		select {
		case <-stop:
			s.commitFinal() // keep what was on screen at the end
			return
		case <-tick.C:
		case <-s.force:
			forced = true
		}
		s.mu.Lock()
		paused, full := s.paused, s.count >= s.max
		s.mu.Unlock()
		if (paused && !forced) || full {
			continue
		}
		img, err := s.capture()
		if err != nil {
			if err.Error() != lastErr {
				lastErr = err.Error()
				s.status("screenshots: " + lastErr)
			}
			continue
		}
		lastErr = ""
		if forced {
			s.saveImage(img, s.timeline())
			continue
		}
		s.observe(img)
	}
}

// capture grabs the meeting window (found automatically or by title), else the screen.
func (s *shotTaker) capture() (*image.RGBA, error) {
	if s.window != "screen" {
		if s.hwnd == 0 || !windowAlive(s.hwnd) || time.Since(s.hwndAt) > 30*time.Second {
			s.hwndAt = time.Now()
			if w, ok := findWindow(s.window); ok {
				if w.hwnd != s.hwnd {
					s.setWhere("window \"" + shorten(w.title, 40) + "\"")
				}
				s.hwnd = w.hwnd
			} else {
				s.hwnd = 0
			}
		}
		if s.hwnd != 0 {
			img, err := captureWindow(s.hwnd)
			if err == nil || err == errMinimised {
				return img, err
			}
			// some apps can't draw themselves off-screen: fall back to the screen
		}
	}
	s.setWhere(fmt.Sprintf("screen %d", s.display))
	return screenshot.CaptureDisplay(s.display)
}

func (s *shotTaker) setWhere(w string) {
	s.mu.Lock()
	changed := s.where != w
	s.where = w
	s.mu.Unlock()
	if changed {
		s.status("screenshots: watching " + w)
	}
}

// observe tracks the content and saves the last settled frame whenever it changes.
func (s *shotTaker) observe(img *image.RGBA) {
	r, ok := s.an.analyse(img)
	if !ok {
		s.commit("content gone") // e.g. screen sharing stopped
		s.cur = nil
		return
	}
	crop := cropRGBA(img, r)
	th := thumbnail(crop)
	now := s.timeline()
	defer func() {
		if s.cur != nil {
			s.cur.latest, s.cur.latestAt = crop, now
		}
	}()
	c := s.cur
	switch {
	case c == nil || iou(c.rect, r) < 0.7 || diff(th, c.thumb) > 0.08:
		// new content (next slide, cleared whiteboard, different layout)
		s.commit("changed")
		s.cur = &content{rect: r, thumb: th, firstThumb: th, since: time.Now(), frames: 1}
	default:
		// same content, possibly evolving (whiteboard drawing, slide build, scrolling)
		if diff(th, c.thumb) < 0.015 { // two frames alike = settled
			c.settled, c.settledAt = crop, now
		}
		c.thumb, c.rect = th, r
		c.frames++
		// a slow, steady change (long whiteboard session, scrolling a document):
		// keep a copy every few minutes so nothing is lost
		if diff(th, c.firstThumb) > 0.25 && time.Since(c.since) > 3*time.Minute {
			s.commit("long session")
			s.cur = &content{rect: r, thumb: th, firstThumb: th, since: time.Now(), frames: 1, settled: crop, settledAt: now}
		}
	}
}

// commit saves the current content's last settled frame, unless it's a near-duplicate.
func (s *shotTaker) commit(_ string) {
	c := s.cur
	if c == nil || c.settled == nil || c.frames < 2 {
		return
	}
	th := thumbnail(c.settled)
	for _, prev := range s.saved { // same view as an earlier shot (e.g. scrolled back)
		if diff(th, prev) < 0.06 {
			c.settled = nil
			return
		}
	}
	s.saveImage(c.settled, c.settledAt)
	s.saved = append(s.saved, th)
	c.settled = nil
}

// commitFinal: at the end of the recording keep what's on screen even if it was only
// seen once (the screen is usually showing something meaningful when you stop).
func (s *shotTaker) commitFinal() {
	c := s.cur
	if c != nil && c.settled == nil && c.latest != nil {
		c.settled, c.settledAt, c.frames = c.latest, c.latestAt, max(c.frames, 2)
	}
	s.commit("end of meeting")
}

func (s *shotTaker) takeNow() {
	select {
	case s.force <- struct{}{}:
	default:
	}
}

func (s *shotTaker) togglePause() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = !s.paused
	return s.paused
}

func (s *shotTaker) taken() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// saveImage writes shot-NNN-tSECONDS.jpg (preview, <= 1024 px wide) and
// shot-NNN-tSECONDS.full.jpg (full resolution, high quality).
func (s *shotTaker) saveImage(img *image.RGBA, at float64) {
	s.mu.Lock()
	s.count++
	n := s.count
	s.mu.Unlock()
	base := filepath.Join(s.dir, fmt.Sprintf("shot-%03d-t%07.1f", n, at))
	if err := writeJPEG(base+".full.jpg", img, 90); err != nil {
		s.status("screenshot save failed: " + err.Error())
		return
	}
	if err := writeJPEG(base+".jpg", resizeTo(img, 1024), 72); err != nil {
		s.status("screenshot save failed: " + err.Error())
	}
}

func resizeTo(img image.Image, maxW int) image.Image {
	b := img.Bounds()
	if b.Dx() <= maxW {
		return img
	}
	h := b.Dy() * maxW / b.Dx()
	dst := image.NewRGBA(image.Rect(0, 0, maxW, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}

func writeJPEG(path string, img image.Image, quality int) error {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return err
	}
	return os.WriteFile(path, withJFIF(buf.Bytes()), 0o644)
}

// withJFIF adds the standard JFIF header that Go's encoder leaves out; some tools
// (e.g. Word export) don't recognise a JPEG without it.
func withJFIF(j []byte) []byte {
	if len(j) < 4 || j[0] != 0xFF || j[1] != 0xD8 || (j[2] == 0xFF && (j[3] == 0xE0 || j[3] == 0xE1)) {
		return j
	}
	app0 := []byte{0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00}
	return append(append(append([]byte{}, j[:2]...), app0...), j[2:]...)
}

func cropRGBA(img *image.RGBA, r image.Rectangle) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Copy(out, image.Point{}, img, r, draw.Src, nil)
	return out
}

// thumbnail averages an image into thumbW x thumbH grayscale cells (0..1).
func thumbnail(img *image.RGBA) []float32 {
	b := img.Bounds()
	out := make([]float32, thumbW*thumbH)
	cw, ch := b.Dx()/thumbW, b.Dy()/thumbH
	if cw == 0 || ch == 0 {
		return out
	}
	for ty := 0; ty < thumbH; ty++ {
		for tx := 0; tx < thumbW; tx++ {
			var sum, n float64
			for y := ty * ch; y < (ty+1)*ch; y += 3 {
				for x := tx * cw; x < (tx+1)*cw; x += 3 {
					i := img.PixOffset(b.Min.X+x, b.Min.Y+y)
					p := img.Pix[i : i+3]
					sum += 0.299*float64(p[0]) + 0.587*float64(p[1]) + 0.114*float64(p[2])
					n++
				}
			}
			out[ty*thumbW+tx] = float32(sum / n / 255)
		}
	}
	return out
}

// diff is the share of thumbnail cells that changed noticeably.
func diff(a, b []float32) float64 {
	changed := 0
	for i := range a {
		if math.Abs(float64(a[i]-b[i])) > 0.08 {
			changed++
		}
	}
	return float64(changed) / float64(len(a))
}

func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func displayCount() int { return screenshot.NumActiveDisplays() }

func displayInfo(i int) string {
	r := screenshot.GetDisplayBounds(i)
	return fmt.Sprintf("display %d: %dx%d at (%d,%d)", i, r.Dx(), r.Dy(), r.Min.X, r.Min.Y)
}
