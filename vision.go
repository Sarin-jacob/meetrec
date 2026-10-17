package main

// Finding the shared presentation / whiteboard inside a meeting window, on the device,
// with cheap image processing plus a tiny face detector (pigo, pure Go):
//
//  1. Layout: meeting apps draw video tiles and the shared screen as rectangles, so the
//     window has long straight edges at tile borders. Their positions are the layout
//     "signature"; when it changes (someone starts sharing, gallery <-> speaker view)
//     the layout is re-analysed.
//  2. Faces are detected only when the layout changes (or every 30 s), not on every
//     frame. Each face marks the tile around it as "video".
//  3. Motion: a grid remembers which areas keep changing (live video).
//  4. Content = the largest rectangle that is neither video nor constantly moving and
//     has some texture (text, drawings, charts). No such area = nothing worth a shot.

import (
	_ "embed"
	"image"
	"math"
	"sort"
	"time"

	pigo "github.com/esimov/pigo/core"
)

//go:embed assets/facefinder
var faceCascade []byte

const workW = 640 // analysis width; screenshots themselves keep full resolution
const cell = 16   // motion / layout grid cell size at workW

type analyzer struct {
	faces      *pigo.Pigo
	lines      layout
	faceBoxes  []image.Rectangle // in work coordinates
	facesAt    time.Time
	prev       []uint8
	motion     []float32
	gw, gh     int
	w, h       int
	frames     int             // captures analysed
	sinceShift int             // captures since the layout last changed
	content    image.Rectangle // last content area found (work coordinates)
	LastReason string
}

// line is a long straight edge: its position and the span it covers.
type line struct{ pos, from, to int }

type layout struct {
	rows, cols []line // long horizontal / vertical edges (work coordinates)
}

func positions(ls []line) []int {
	out := make([]int, len(ls))
	for i, l := range ls {
		out[i] = l.pos
	}
	return out
}

func newAnalyzer() *analyzer {
	cls, err := pigo.NewPigo().Unpack(faceCascade)
	if err != nil {
		panic("face model: " + err.Error())
	}
	return &analyzer{faces: cls}
}

// toGray scales the image to workW wide and converts it to grayscale.
func toGray(img *image.RGBA) (g []uint8, w, h int, scale float64) {
	b := img.Bounds()
	scale = float64(b.Dx()) / workW
	if scale < 1 {
		scale = 1
	}
	w, h = int(float64(b.Dx())/scale), int(float64(b.Dy())/scale)
	g = make([]uint8, w*h)
	for y := 0; y < h; y++ {
		sy := b.Min.Y + int(float64(y)*scale)
		for x := 0; x < w; x++ {
			i := img.PixOffset(b.Min.X+int(float64(x)*scale), sy)
			p := img.Pix[i : i+3]
			g[y*w+x] = uint8((299*int(p[0]) + 587*int(p[1]) + 114*int(p[2])) / 1000)
		}
	}
	return g, w, h, scale
}

// detectLayout finds rows/columns that have a long straight run of strong edges:
// tile borders, the shared-screen frame, toolbars.
func detectLayout(g []uint8, w, h int) layout {
	var l layout
	const strong = 24
	for y := 1; y < h-1; y++ {
		run, best, bestEnd := 0, 0, 0
		for x := 0; x < w; x++ {
			if absI(int(g[y*w+x])-int(g[(y-1)*w+x])) > strong {
				run++
				if run > best {
					best, bestEnd = run, x
				}
			} else {
				run = 0
			}
		}
		if best > w/4 {
			l.rows = append(l.rows, line{y, bestEnd - best + 1, bestEnd})
		}
	}
	for x := 1; x < w-1; x++ {
		run, best, bestEnd := 0, 0, 0
		for y := 0; y < h; y++ {
			if absI(int(g[y*w+x])-int(g[y*w+x-1])) > strong {
				run++
				if run > best {
					best, bestEnd = run, y
				}
			} else {
				run = 0
			}
		}
		if best > h/4 {
			l.cols = append(l.cols, line{x, bestEnd - best + 1, bestEnd})
		}
	}
	l.rows, l.cols = mergeLines(l.rows), mergeLines(l.cols)
	return l
}

func mergeLines(v []line) []line { // neighbouring edges (within 3 px) count as one line
	var out []line
	for _, l := range v {
		if len(out) == 0 || l.pos-out[len(out)-1].pos > 3 {
			out = append(out, l)
		}
	}
	return out
}

// outside drops lines lying inside r (the shared content), so a new slide's title bar
// or long text lines don't count as a layout change.
func (l layout) outside(r image.Rectangle) layout {
	in := r.Inset(4)
	if in.Empty() {
		return l
	}
	var out layout
	for _, x := range l.rows {
		if !(x.pos > in.Min.Y && x.pos < in.Max.Y && x.from >= in.Min.X-8 && x.to <= in.Max.X+8) {
			out.rows = append(out.rows, x)
		}
	}
	for _, x := range l.cols {
		if !(x.pos > in.Min.X && x.pos < in.Max.X && x.from >= in.Min.Y-8 && x.to <= in.Max.Y+8) {
			out.cols = append(out.cols, x)
		}
	}
	return out
}

func sameLayout(a, b layout) bool {
	return sameLines(positions(a.rows), positions(b.rows)) && sameLines(positions(a.cols), positions(b.cols))
}

func sameLines(a, b []int) bool {
	if absI(len(a)-len(b)) > 1 {
		return false
	}
	matched := 0
	for _, x := range a {
		for _, y := range b {
			if absI(x-y) <= 4 {
				matched++
				break
			}
		}
	}
	return matched >= max(len(a), len(b))-1
}

// analyse returns the content rectangle in the image's own (full-resolution) coordinates.
//
// Every cell of a grid over the window is classified as
//   - background: the meeting app's own flat backdrop (its most common flat shade),
//   - video:      a face's tile, an area that keeps changing, or camera-like pixels,
//   - candidate:  anything else (documents, slides, whiteboards, but also toolbars and
//     name labels).
//
// The shared content is the largest connected block of candidate cells: toolbars and
// labels are separated from it by background, so they form their own small blocks.
func (a *analyzer) analyse(img *image.RGBA) (image.Rectangle, bool) {
	g, w, h, scale := toGray(img)
	if w != a.w || h != a.h { // window resized: start over
		a.w, a.h = w, h
		a.gw, a.gh = (w+cell-1)/cell, (h+cell-1)/cell
		a.motion = make([]float32, a.gw*a.gh)
		a.prev = nil
		a.frames = 0
		a.facesAt = time.Time{}
	}

	// layout lines (tile borders) and faces: re-detected when the layout changes, or every 10 s
	lay := detectLayout(g, w, h)
	shifted := !sameLayout(lay.outside(a.content), a.lines.outside(a.content))
	a.lines = lay
	if shifted || time.Since(a.facesAt) > 10*time.Second {
		a.faceBoxes = a.findFaces(g, w, h)
		a.facesAt = time.Now()
	}

	// motion map
	if a.prev != nil {
		for cy := 0; cy < a.gh; cy++ {
			for cx := 0; cx < a.gw; cx++ {
				var sum, n int
				for y := cy * cell; y < min((cy+1)*cell, h); y += 2 {
					for x := cx * cell; x < min((cx+1)*cell, w); x += 2 {
						sum += absI(int(g[y*w+x]) - int(a.prev[y*w+x]))
						n++
					}
				}
				// video changes moderately all the time; a window switch or a new slide
				// changes a cell completely, once: that's content changing, not video
				changed := float32(0)
				if d := sum / max(n, 1); d > 6 && d < 60 {
					changed = 1
				}
				i := cy*a.gw + cx
				a.motion[i] = 0.7*a.motion[i] + 0.3*changed
			}
		}
	}
	a.prev = g
	a.frames++
	// live video is only recognisable after a few captures (it keeps changing)
	if a.frames < 3 {
		a.LastReason = "learning the layout"
		return image.Rectangle{}, false
	}

	// classify cells
	n := a.gw * a.gh
	video := make([]bool, n)
	for _, t := range a.videoTiles(w, h) {
		for cy := max(t.Min.Y/cell, 0); cy <= min(t.Max.Y/cell, a.gh-1); cy++ {
			for cx := max(t.Min.X/cell, 0); cx <= min(t.Max.X/cell, a.gw-1); cx++ {
				video[cy*a.gw+cx] = true
			}
		}
	}
	photo := photoCells(g, w, h, a.gw, a.gh)
	bg := backgroundCells(g, w, h, a.gw, a.gh)
	cand := make([]bool, n)
	for i := range cand {
		cand[i] = !bg[i] && !video[i] && !photo[i]
	}

	// Trim 2 cells off every block's edges before finding the largest one: this cuts thin
	// links (a floating toolbar button, a strip of window frame) that would otherwise join
	// the shared content to the app's title bar or toolbar. Then add the 2 cells back.
	core := erode(erode(cand, a.gw, a.gh), a.gw, a.gh)
	// Motion is judged per block, not per cell: a camera tile that face detection and the
	// photo check both missed is mostly moving; a document being scrolled only has
	// scattered changing cells. Take the largest block that isn't mostly moving.
	var r image.Rectangle
	cells := 0
	for _, b := range blocks(core, a.gw, a.gh) {
		moving := 0
		for _, i := range b.cells {
			if a.motion[i] >= 0.6 {
				moving++
			}
		}
		if moving*2 <= len(b.cells) {
			r, cells = b.box, len(b.cells)
			break
		}
	}
	// add the trimmed edge back, but only as far as real candidate cells reach
	grow := image.Rect(max(r.Min.X-2, 0), max(r.Min.Y-2, 0), min(r.Max.X+2, a.gw), min(r.Max.Y+2, a.gh))
	var tight image.Rectangle
	for cy := grow.Min.Y; cy < grow.Max.Y; cy++ {
		for cx := grow.Min.X; cx < grow.Max.X; cx++ {
			if cand[cy*a.gw+cx] {
				tight = tight.Union(image.Rect(cx, cy, cx+1, cy+1))
			}
		}
	}
	if !tight.Empty() {
		r = tight
	}
	cells += 2 * (r.Dx() + r.Dy())
	r = image.Rect(r.Min.X*cell, r.Min.Y*cell, min(r.Max.X*cell, w), min(r.Max.Y*cell, h))
	if cells*cell*cell < w*h/10 {
		a.content = image.Rectangle{}
		a.LastReason = "no shared content visible"
		return image.Rectangle{}, false
	}
	r = a.snap(r)
	a.content = r
	if texture(g, w, r) < 0.004 && ink(g, w, r) < 0.004 {
		a.LastReason = "content area is blank"
		return image.Rectangle{}, false
	}
	a.LastReason = ""
	b := img.Bounds()
	full := image.Rect(int(float64(r.Min.X)*scale), int(float64(r.Min.Y)*scale),
		int(float64(r.Max.X)*scale), int(float64(r.Max.Y)*scale)).Add(b.Min).Intersect(b)
	return full, true
}

// backgroundCells marks cells that are the meeting app's backdrop: the flat dark shade
// most common along the window's edges (Zoom, Teams and Meet all put the meeting on a
// dark stage, and that stage surrounds everything). With no dark backdrop (e.g. a
// full-screen slide) nothing is marked.
func backgroundCells(g []uint8, w, h, gw, gh int) []bool {
	means := make([]int, gw*gh)
	flat := make([]bool, gw*gh)
	for cy := 0; cy < gh; cy++ {
		for cx := 0; cx < gw; cx++ {
			var n, sum, sum2 int
			for y := cy * cell; y < min((cy+1)*cell, h); y += 2 {
				for x := cx * cell; x < min((cx+1)*cell, w); x += 2 {
					v := int(g[y*w+x])
					sum += v
					sum2 += v * v
					n++
				}
			}
			if n == 0 {
				continue
			}
			mean := float64(sum) / float64(n)
			i := cy*gw + cx
			means[i] = int(mean)
			flat[i] = math.Sqrt(float64(sum2)/float64(n)-mean*mean) < 4
		}
	}
	var hist [32]int // flat dark shades along the edge (outer 2 rings of cells)
	ring := 0
	for cy := 0; cy < gh; cy++ {
		for cx := 0; cx < gw; cx++ {
			if cx > 1 && cy > 1 && cx < gw-2 && cy < gh-2 {
				continue
			}
			ring++
			if i := cy*gw + cx; flat[i] && means[i] < 80 {
				hist[means[i]/8]++
			}
		}
	}
	top := 0
	for i := range hist {
		if hist[i] > hist[top] {
			top = i
		}
	}
	out := make([]bool, gw*gh)
	if hist[top]*5 < ring { // under 20% of the edge: no backdrop to speak of
		return out
	}
	shade := top*8 + 4
	for i := range out {
		out[i] = flat[i] && absI(means[i]-shade) <= 10
	}
	return out
}

// erode keeps a cell only if its 4 neighbours are set too (outside the grid counts as unset).
func erode(m []bool, gw, gh int) []bool {
	out := make([]bool, len(m))
	for y := 1; y < gh-1; y++ {
		for x := 1; x < gw-1; x++ {
			i := y*gw + x
			out[i] = m[i] && m[i-1] && m[i+1] && m[i-gw] && m[i+gw]
		}
	}
	return out
}

type block struct {
	box   image.Rectangle // in cells
	cells []int
}

// blocks returns the groups of connected cells, largest first.
func blocks(mask []bool, gw, gh int) []block {
	seen := make([]bool, len(mask))
	var out []block
	for start := range mask {
		if !mask[start] || seen[start] {
			continue
		}
		var b block
		stack := []int{start}
		seen[start] = true
		for len(stack) > 0 {
			i := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := i%gw, i/gw
			b.cells = append(b.cells, i)
			b.box = b.box.Union(image.Rect(x, y, x+1, y+1))
			for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				nx, ny := x+d[0], y+d[1]
				if nx >= 0 && ny >= 0 && nx < gw && ny < gh {
					if j := ny*gw + nx; mask[j] && !seen[j] {
						seen[j] = true
						stack = append(stack, j)
					}
				}
			}
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].cells) > len(out[j].cells) })
	return out
}

// largestBlock: bounding box (in cells) and size of the biggest connected group.
func largestBlock(mask []bool, gw, gh int) (image.Rectangle, int) {
	bs := blocks(mask, gw, gh)
	if len(bs) == 0 {
		return image.Rectangle{}, 0
	}
	return bs[0].box, len(bs[0].cells)
}

func (a *analyzer) findFaces(g []uint8, w, h int) []image.Rectangle {
	dets := a.faces.RunCascade(pigo.CascadeParams{
		MinSize: 18, MaxSize: min(w, h), ShiftFactor: 0.1, ScaleFactor: 1.1,
		ImageParams: pigo.ImageParams{Pixels: g, Rows: h, Cols: w, Dim: w},
	}, 0)
	dets = a.faces.ClusterDetections(dets, 0.2)
	var out []image.Rectangle
	for _, d := range dets {
		if d.Q < 5 {
			continue
		}
		s := d.Scale / 2
		out = append(out, image.Rect(d.Col-s, d.Row-s, d.Col+s, d.Row+s))
	}
	return out
}

// videoTiles grows each face into the tile it sits in: up to the nearest layout lines,
// or a generous box around the face when the layout has no lines there.
func (a *analyzer) videoTiles(w, h int) []image.Rectangle {
	var tiles []image.Rectangle
	for _, f := range a.faceBoxes {
		cx, cy, s := (f.Min.X+f.Max.X)/2, (f.Min.Y+f.Max.Y)/2, f.Dx()
		left, right := lineAround(positions(a.lines.cols), cx, cx-3*s, cx+3*s)
		top, bottom := lineAround(positions(a.lines.rows), cy, cy-2*s, cy+4*s)
		tiles = append(tiles, image.Rect(max(left, 0), max(top, 0), min(right, w), min(bottom, h)))
	}
	return tiles
}

// lineAround: the nearest line before and after p (within 4x the fallback distance).
func lineAround(lines []int, p, lo, hi int) (int, int) {
	before, after := lo, hi
	reach := 4 * (hi - lo)
	for _, l := range lines {
		if l < p && p-l < reach && (before == lo || l > before) {
			before = l
		}
		if l > p && l-p < reach && (after == hi || l < after) {
			after = l
		}
	}
	return before, after
}

// snap moves each edge of r to a layout line within 1.5 cells, for a clean crop.
func (a *analyzer) snap(r image.Rectangle) image.Rectangle {
	near := func(lines []int, v int, inward int) int {
		for _, l := range lines {
			if absI(l-v) <= cell*3/2 && (l-v)*inward >= -cell/2 {
				return l
			}
		}
		return v
	}
	cols, rows := positions(a.lines.cols), positions(a.lines.rows)
	r.Min.X = near(cols, r.Min.X, 1)
	r.Max.X = near(cols, r.Max.X, -1)
	r.Min.Y = near(rows, r.Min.Y, 1)
	r.Max.Y = near(rows, r.Max.Y, -1)
	return r.Canon()
}

// texture: share of pixels with a visible edge (text, lines, charts). Blank areas ~0.
func texture(g []uint8, w int, r image.Rectangle) float64 {
	var edges, n int
	for y := r.Min.Y + 1; y < r.Max.Y; y += 2 {
		for x := r.Min.X + 1; x < r.Max.X; x += 2 {
			if absI(int(g[y*w+x])-int(g[y*w+x-1]))+absI(int(g[y*w+x])-int(g[(y-1)*w+x])) > 40 {
				edges++
			}
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(edges) / float64(n)
}

// photoCells marks grid cells that look like camera video rather than a document:
// slides, text, whiteboards and UI are mostly one flat background shade with marks on
// it; camera images spread over many tones. Works on a single frame, so it helps even
// before motion has been learned (and on video that happens to be still).
func photoCells(g []uint8, w, h, gw, gh int) []bool {
	out := make([]bool, gw*gh)
	for cy := 0; cy < gh; cy++ {
		for cx := 0; cx < gw; cx++ {
			var hist [32]int
			var n, sum, sum2 int
			for y := cy * cell; y < min((cy+1)*cell, h); y++ {
				for x := cx * cell; x < min((cx+1)*cell, w); x++ {
					v := int(g[y*w+x])
					hist[v/8]++
					sum += v
					sum2 += v * v
					n++
				}
			}
			if n == 0 {
				continue
			}
			best := 0 // most common band of 3 neighbouring bins (+-12 grey levels)
			for i := 0; i < 32; i++ {
				c := hist[i]
				if i > 0 {
					c += hist[i-1]
				}
				if i < 31 {
					c += hist[i+1]
				}
				if c > best {
					best = c
				}
			}
			mean := float64(sum) / float64(n)
			std := math.Sqrt(float64(sum2)/float64(n) - mean*mean)
			tones := 0 // distinct tone bands with a real share (camera images: many)
			for _, c := range hist {
				if c*50 >= n { // >= 2%
					tones++
				}
			}
			out[cy*gw+cx] = float64(best)/float64(n) < 0.55 && std > 14 && tones >= 6
		}
	}
	// a single photo-like cell inside a slide (a small picture) shouldn't split it:
	// only keep cells that have at least 2 photo-like neighbours
	keep := make([]bool, len(out))
	for cy := 0; cy < gh; cy++ {
		for cx := 0; cx < gw; cx++ {
			if !out[cy*gw+cx] {
				continue
			}
			nb := 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					x, y := cx+dx, cy+dy
					if (dx != 0 || dy != 0) && x >= 0 && y >= 0 && x < gw && y < gh && out[y*gw+x] {
						nb++
					}
				}
			}
			keep[cy*gw+cx] = nb >= 2
		}
	}
	return keep
}

// ink: share of pixels clearly different from the area's background (its most common
// shade). Catches sparse content like a few whiteboard strokes.
func ink(g []uint8, w int, r image.Rectangle) float64 {
	var hist [32]int
	var n int
	for y := r.Min.Y; y < r.Max.Y; y += 2 {
		for x := r.Min.X; x < r.Max.X; x += 2 {
			hist[g[y*w+x]/8]++
			n++
		}
	}
	if n == 0 {
		return 0
	}
	bg := 0
	for i := range hist {
		if hist[i] > hist[bg] {
			bg = i
		}
	}
	far := 0
	for i := range hist {
		if absI(i-bg) >= 5 { // 40+ grey levels away from the background
			far += hist[i]
		}
	}
	return float64(far) / float64(n)
}

// largestRect finds the largest all-true rectangle in a grid (histogram method).
func largestRect(ok []bool, gw, gh int) image.Rectangle {
	heights := make([]int, gw)
	var best image.Rectangle
	bestArea := 0
	for y := 0; y < gh; y++ {
		for x := 0; x < gw; x++ {
			if ok[y*gw+x] {
				heights[x]++
			} else {
				heights[x] = 0
			}
		}
		var stack []int
		for x := 0; x <= gw; x++ {
			hcur := 0
			if x < gw {
				hcur = heights[x]
			}
			for len(stack) > 0 && heights[stack[len(stack)-1]] >= hcur {
				top := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				left := 0
				if len(stack) > 0 {
					left = stack[len(stack)-1] + 1
				}
				if area := heights[top] * (x - left); area > bestArea && heights[top] > 0 {
					bestArea = area
					best = image.Rect(left, y-heights[top]+1, x, y+1)
				}
			}
			stack = append(stack, x)
		}
	}
	return best
}

func absI(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// iou of two rectangles (0..1)
func iou(a, b image.Rectangle) float64 {
	in := a.Intersect(b)
	if in.Empty() {
		return 0
	}
	ia := float64(in.Dx() * in.Dy())
	return ia / (float64(a.Dx()*a.Dy()) + float64(b.Dx()*b.Dy()) - ia)
}
