package main

import (
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// fakeMeeting draws a 1280x720 meeting window: a shared slide/whiteboard on the left
// (white, with `lines` dark text bars) and three video tiles on the right whose pixels
// change every frame like live video.
var slideRect = image.Rect(48, 48, 880, 672) // the app's dark backdrop shows around everything

func fakeMeeting(rng *rand.Rand, lines int, whiteboard bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	fill := func(r image.Rectangle, c color.RGBA) {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				img.SetRGBA(x, y, c)
			}
		}
	}
	fill(img.Bounds(), color.RGBA{36, 36, 40, 255})
	fill(slideRect, color.RGBA{250, 250, 250, 255})
	for i := 0; i < lines; i++ {
		y := 60 + i*70
		if whiteboard { // strokes added one by one
			fill(image.Rect(60+i*20, y, 500+i*30, y+10), color.RGBA{20, 70, 200, 255})
		} else {
			fill(image.Rect(60, y, 820-(i*37)%300, y+22), color.RGBA{30, 30, 30, 255})
		}
	}
	for t := 0; t < 3; t++ { // video tiles with a border and noisy content
		tile := image.Rect(944, 48+t*208, 1232, 240+t*208)
		fill(tile, color.RGBA{200, 200, 200, 255})
		inner := tile.Inset(3)
		for y := inner.Min.Y; y < inner.Max.Y; y++ {
			for x := inner.Min.X; x < inner.Max.X; x++ {
				v := uint8(rng.Intn(200))
				img.SetRGBA(x, y, color.RGBA{v, v / 2, 255 - v, 255})
			}
		}
	}
	return img
}

func TestFindsSharedContentNotVideo(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	a := newAnalyzer()
	var r image.Rectangle
	var ok bool
	for i := 0; i < 5; i++ {
		r, ok = a.analyse(fakeMeeting(rng, 5, false))
	}
	if !ok {
		t.Fatalf("no content found: %s", a.LastReason)
	}
	if v := iou(r, slideRect); v < 0.85 {
		t.Fatalf("content %v vs slide %v: IoU %.2f", r, slideRect, v)
	}
	if r.Max.X > 920 {
		t.Fatalf("crop includes video tiles: %v", r)
	}
}

func TestOneShotPerSlideAndWhiteboardBeforeWipe(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	dir := t.TempDir()
	clock := 0.0
	s := newShotTaker(dir, 0, "screen", func() float64 { return clock }, func(string) {})
	feed := func(lines, frames int, wb bool) {
		for i := 0; i < frames; i++ {
			clock += 3
			s.observe(fakeMeeting(rng, lines, wb))
		}
	}
	feed(3, 6, false) // slide 1 (the first few captures learn the layout)
	feed(7, 4, false) // slide 2
	feed(2, 4, false) // slide 3
	if got := s.taken(); got != 2 {
		t.Fatalf("after 3 slides (last still showing) want 2 shots, got %d", got)
	}
	// whiteboard: strokes added one at a time, then wiped
	for n := 1; n <= 6; n++ {
		feed(n, 2, true)
	}
	feed(0, 3, true) // wiped: blank board
	files, _ := filepath.Glob(filepath.Join(dir, "*.full.jpg"))
	if len(files) != 4 { // slide 1, 2, 3, finished whiteboard
		t.Fatalf("want 4 shots, got %d: %v", len(files), files)
	}
	// the whiteboard shot must be the finished board (6 strokes), not a partial one
	f, _ := os.Open(files[3])
	img, err := jpeg.Decode(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	// stroke 6 (the last one drawn) sits at y=410..420, x=160..650 in the window;
	// the shot starts at (about) the content's top-left corner
	r, _, b, _ := img.At(400-slideRect.Min.X, 415-slideRect.Min.Y).RGBA()
	if !(b>>8 > 150 && r>>8 < 80) {
		t.Fatalf("whiteboard shot is missing the last stroke (pixel r=%d b=%d)", r>>8, b>>8)
	}
}

func TestSavesPreviewAndFull(t *testing.T) {
	dir := t.TempDir()
	s := newShotTaker(dir, 0, "screen", func() float64 { return 123.4 }, func(string) {})
	s.saveImage(fakeMeeting(rand.New(rand.NewSource(3)), 4, false), 123.4)
	for name, wantW := range map[string]int{"shot-001-t00123.4.jpg": 1024, "shot-001-t00123.4.full.jpg": 1280} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := jpeg.DecodeConfig(f)
		f.Close()
		if err != nil || cfg.Width != wantW {
			t.Fatalf("%s: want width %d, got %v %v", name, wantW, cfg.Width, err)
		}
	}
}

func TestJFIF(t *testing.T) {
	out := withJFIF([]byte{0xFF, 0xD8, 0xFF, 0xDB, 1, 2})
	if string(out[6:10]) != "JFIF" || out[2] != 0xFF || out[3] != 0xE0 {
		t.Fatalf("no JFIF header: % x", out[:12])
	}
	if got := withJFIF(out); len(got) != len(out) {
		t.Fatal("header added twice")
	}
}

func TestLargestRect(t *testing.T) {
	// 6x4 grid with a 4x3 block of true in the top-left
	ok := make([]bool, 24)
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			ok[y*6+x] = true
		}
	}
	if r := largestRect(ok, 6, 4); r != image.Rect(0, 0, 4, 3) {
		t.Fatalf("got %v", r)
	}
}
