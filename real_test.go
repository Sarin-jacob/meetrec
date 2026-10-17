package main

import (
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"testing"
)

// MEETREC_IMAGES=dir go test -run TestRealImages -v
// Runs the content finder on still meeting screenshots and writes annotated copies
// (faces red, chosen crop green) to dir/out.
func TestRealImages(t *testing.T) {
	dir := os.Getenv("MEETREC_IMAGES")
	if dir == "" {
		t.Skip("set MEETREC_IMAGES")
	}
	out := filepath.Join(dir, "out")
	os.MkdirAll(out, 0o755)
	files, _ := filepath.Glob(filepath.Join(dir, "*.*"))
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		src, _, err := image.Decode(fh)
		fh.Close()
		if err != nil {
			continue
		}
		img := image.NewRGBA(src.Bounds())
		draw.Draw(img, img.Bounds(), src, src.Bounds().Min, draw.Src)
		a := newAnalyzer()
		var r image.Rectangle
		var ok bool
		for i := 0; i < 5; i++ { // a still image: no motion, so faces + layout decide
			r, ok = a.analyse(img)
		}
		_, w, _, scale := toGray(img)
		_ = w
		t.Logf("%-24s faces=%d content=%v rect=%v reason=%q", filepath.Base(f), len(a.faceBoxes), ok, r, a.LastReason)
		box := func(rr image.Rectangle, c color.RGBA, th int) {
			for y := rr.Min.Y; y < rr.Max.Y; y++ {
				for x := rr.Min.X; x < rr.Max.X; x++ {
					if x-rr.Min.X < th || rr.Max.X-x <= th || y-rr.Min.Y < th || rr.Max.Y-y <= th {
						if (image.Point{x, y}).In(img.Bounds()) {
							img.SetRGBA(x, y, c)
						}
					}
				}
			}
		}
		for _, fb := range a.faceBoxes {
			box(image.Rect(int(float64(fb.Min.X)*scale), int(float64(fb.Min.Y)*scale), int(float64(fb.Max.X)*scale), int(float64(fb.Max.Y)*scale)), color.RGBA{255, 0, 0, 255}, 4)
		}
		if ok {
			box(r, color.RGBA{0, 220, 0, 255}, 8)
		}
		writeJPEG(filepath.Join(out, filepath.Base(f)+".jpg"), resizeTo(img, 1000), 80)
	}
}
