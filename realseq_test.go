package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/png"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"
)

// MEETREC_FRAMES=dir go test -run TestRealSequence -v
// Feeds frames named f###-t###.#.png (one every few seconds of a real meeting) through
// the screenshot logic in order. Shots go to dir/shots, annotated frames to dir/annot.
func TestRealSequence(t *testing.T) {
	dir := os.Getenv("MEETREC_FRAMES")
	if dir == "" {
		t.Skip("set MEETREC_FRAMES")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "f*.png"))
	sort.Strings(files)
	re := regexp.MustCompile(`-t([\d.]+)\.png$`)
	clock := 0.0
	shotsDir := filepath.Join(dir, "shots")
	annot := filepath.Join(dir, "annot")
	os.RemoveAll(shotsDir)
	os.MkdirAll(annot, 0o755)
	s := newShotTaker(shotsDir, 0, "screen", func() float64 { return clock }, func(m string) { t.Log(m) })
	os.MkdirAll(shotsDir, 0o755)
	for _, f := range files {
		m := re.FindStringSubmatch(f)
		clock, _ = strconv.ParseFloat(m[1], 64)
		fh, _ := os.Open(f)
		src, _, err := image.Decode(fh)
		fh.Close()
		if err != nil {
			t.Fatal(err)
		}
		img := image.NewRGBA(src.Bounds())
		draw.Draw(img, img.Bounds(), src, image.Point{}, draw.Src)
		before := s.taken()
		var prevThumb []float32
		if s.cur != nil {
			prevThumb = s.cur.thumb
		}
		s.observe(img)
		extra := ""
		if s.cur != nil {
			d := -1.0
			if prevThumb != nil {
				d = diff(s.cur.thumb, prevThumb)
			}
			extra = fmt.Sprintf(" [frames=%d settled=%v diffPrev=%.3f]", s.cur.frames, s.cur.settled != nil, d)
		}
		var r image.Rectangle
		if s.cur != nil {
			r = s.cur.rect
		}
		t.Logf("%s t=%5.1f faces=%d content=%v reason=%q shots=%d%s", filepath.Base(f), clock, len(s.an.faceBoxes), r, s.an.LastReason, s.taken(),
			map[bool]string{true: "  <- saved", false: ""}[s.taken() > before]+extra)
		// annotate
		_, _, _, scale := toGray(img)
		box := func(rr image.Rectangle, c color.RGBA, th int) {
			for y := rr.Min.Y; y < rr.Max.Y; y++ {
				for x := rr.Min.X; x < rr.Max.X; x++ {
					if (x-rr.Min.X < th || rr.Max.X-x <= th || y-rr.Min.Y < th || rr.Max.Y-y <= th) && (image.Point{x, y}).In(img.Bounds()) {
						img.SetRGBA(x, y, c)
					}
				}
			}
		}
		for _, fb := range s.an.faceBoxes {
			box(image.Rect(int(float64(fb.Min.X)*scale), int(float64(fb.Min.Y)*scale), int(float64(fb.Max.X)*scale), int(float64(fb.Max.Y)*scale)), color.RGBA{255, 0, 0, 255}, 4)
		}
		if !r.Empty() {
			box(r, color.RGBA{0, 220, 0, 255}, 8)
		}
		writeJPEG(filepath.Join(annot, fmt.Sprintf("%s.jpg", filepath.Base(f))), resizeTo(img, 960), 75)
	}
	s.commitFinal()
	t.Logf("total shots: %d", s.taken())
}
