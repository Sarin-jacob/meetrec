//go:build windows

package main

// Capturing a single window (the meeting app) instead of a whole screen. PrintWindow
// with PW_RENDERFULLCONTENT asks the window to draw itself, so it works even when the
// meeting window is behind other windows (but not when it's minimised).

import (
	"errors"
	"image"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	pEnumWindows            = user32.NewProc("EnumWindows")
	pGetWindowTextW         = user32.NewProc("GetWindowTextW")
	pGetWindowTextLengthW   = user32.NewProc("GetWindowTextLengthW")
	pIsWindowVisible        = user32.NewProc("IsWindowVisible")
	pIsIconic               = user32.NewProc("IsIconic")
	pIsWindow               = user32.NewProc("IsWindow")
	pGetWindowRect          = user32.NewProc("GetWindowRect")
	pPrintWindow            = user32.NewProc("PrintWindow")
	pGetDC                  = user32.NewProc("GetDC")
	pReleaseDC              = user32.NewProc("ReleaseDC")
	pCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	pCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	pSelectObject           = gdi32.NewProc("SelectObject")
	pDeleteObject           = gdi32.NewProc("DeleteObject")
	pDeleteDC               = gdi32.NewProc("DeleteDC")
	pGetDIBits              = gdi32.NewProc("GetDIBits")
)

type rect32 struct{ Left, Top, Right, Bottom int32 }

type window struct {
	hwnd  uintptr
	title string
	w, h  int
}

// Window titles of common meeting apps (lower case). Browser meetings show the tab title.
var meetingTitles = []string{
	"zoom meeting", "zoom workplace", "zoom webinar", "microsoft teams", "meet - ", "google meet",
	"webex", "jitsi meet", "whereby", "skype", "slack huddle", "huddle", "gotomeeting", "bluejeans",
}

func listWindows() []window {
	var out []window
	cb := windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if v, _, _ := pIsWindowVisible.Call(hwnd); v == 0 {
			return 1
		}
		n, _, _ := pGetWindowTextLengthW.Call(hwnd)
		if n == 0 {
			return 1
		}
		buf := make([]uint16, n+1)
		pGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
		var r rect32
		pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
		w, h := int(r.Right-r.Left), int(r.Bottom-r.Top)
		if w >= 400 && h >= 300 {
			out = append(out, window{hwnd: hwnd, title: syscall.UTF16ToString(buf), w: w, h: h})
		}
		return 1
	})
	pEnumWindows.Call(cb, 0)
	return out
}

// findWindow returns the largest visible window whose title contains `want`
// ("auto" = any known meeting app).
func findWindow(want string) (window, bool) {
	want = strings.ToLower(want)
	var best window
	found := false
	for _, w := range listWindows() {
		t := strings.ToLower(w.title)
		match := false
		if want == "auto" {
			for _, m := range meetingTitles {
				if strings.Contains(t, m) {
					match = true
					break
				}
			}
		} else {
			match = strings.Contains(t, want)
		}
		if match && (!found || w.w*w.h > best.w*best.h) {
			best, found = w, true
		}
	}
	return best, found
}

func windowAlive(hwnd uintptr) bool {
	ok, _, _ := pIsWindow.Call(hwnd)
	return ok != 0
}

var errMinimised = errors.New("meeting window is minimised")

func captureWindow(hwnd uintptr) (*image.RGBA, error) {
	if ic, _, _ := pIsIconic.Call(hwnd); ic != 0 {
		return nil, errMinimised
	}
	var r rect32
	pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	w, h := int(r.Right-r.Left), int(r.Bottom-r.Top)
	if w <= 0 || h <= 0 {
		return nil, errors.New("window has no size")
	}
	screen, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, screen)
	mem, _, _ := pCreateCompatibleDC.Call(screen)
	defer pDeleteDC.Call(mem)
	bmp, _, _ := pCreateCompatibleBitmap.Call(screen, uintptr(w), uintptr(h))
	defer pDeleteObject.Call(bmp)
	old, _, _ := pSelectObject.Call(mem, bmp)
	const PW_RENDERFULLCONTENT = 2
	ok, _, _ := pPrintWindow.Call(hwnd, mem, PW_RENDERFULLCONTENT)
	pSelectObject.Call(mem, old)
	if ok == 0 {
		return nil, errors.New("PrintWindow failed")
	}
	type bitmapInfoHeader struct {
		Size                          uint32
		Width, Height                 int32
		Planes, BitCount              uint16
		Compression, SizeImage        uint32
		XPels, YPels, ClrUsed, Clrimp int32
	}
	bi := bitmapInfoHeader{Size: 40, Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
	buf := make([]byte, w*h*4)
	if n, _, _ := pGetDIBits.Call(mem, bmp, 0, uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bi)), 0); n == 0 {
		return nil, errors.New("GetDIBits failed")
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	dark := true
	for i := 0; i < len(buf); i += 4 { // BGRA -> RGBA
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = buf[i+2], buf[i+1], buf[i], 255
		if dark && (buf[i] > 8 || buf[i+1] > 8 || buf[i+2] > 8) {
			dark = false
		}
	}
	if dark {
		return nil, errors.New("window didn't draw (try --window screen)")
	}
	return img, nil
}
