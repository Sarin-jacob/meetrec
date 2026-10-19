//go:build windows

package main

// The floating recorder: a small always-on-top pill. Click it for a menu (start, stop,
// pause, screenshot, language, people, hide). "Hide to taskbar" swaps it for a taskbar
// button; recording carries on. Ctrl+Alt shortcuts work while the pill is showing.
// After stopping, a small window asks for the title, language and people, and uploads
// (or uploads with those defaults by itself after a minute).
//
// Plain Win32 through syscalls: no extra runtime, a few MB of RAM.

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	pRegisterClassExW              = user32.NewProc("RegisterClassExW")
	pCreateWindowExW               = user32.NewProc("CreateWindowExW")
	pDefWindowProcW                = user32.NewProc("DefWindowProcW")
	pDestroyWindow                 = user32.NewProc("DestroyWindow")
	pGetMessageW                   = user32.NewProc("GetMessageW")
	pTranslateMessage              = user32.NewProc("TranslateMessage")
	pDispatchMessageW              = user32.NewProc("DispatchMessageW")
	pIsDialogMessageW              = user32.NewProc("IsDialogMessageW")
	pPostMessageW                  = user32.NewProc("PostMessageW")
	pSendMessageW                  = user32.NewProc("SendMessageW")
	pPostQuitMessage               = user32.NewProc("PostQuitMessage")
	pShowWindow                    = user32.NewProc("ShowWindow")
	pSetWindowPos                  = user32.NewProc("SetWindowPos")
	pInvalidateRect                = user32.NewProc("InvalidateRect")
	pBeginPaint                    = user32.NewProc("BeginPaint")
	pEndPaint                      = user32.NewProc("EndPaint")
	pGetClientRect                 = user32.NewProc("GetClientRect")
	pSetLayeredWindowAttributes    = user32.NewProc("SetLayeredWindowAttributes")
	pSetWindowRgn                  = user32.NewProc("SetWindowRgn")
	pCreatePopupMenu               = user32.NewProc("CreatePopupMenu")
	pAppendMenuW                   = user32.NewProc("AppendMenuW")
	pTrackPopupMenu                = user32.NewProc("TrackPopupMenu")
	pDestroyMenu                   = user32.NewProc("DestroyMenu")
	pSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	pRegisterHotKey                = user32.NewProc("RegisterHotKey")
	pUnregisterHotKey              = user32.NewProc("UnregisterHotKey")
	pSetTimer                      = user32.NewProc("SetTimer")
	pKillTimer                     = user32.NewProc("KillTimer")
	pSetCapture                    = user32.NewProc("SetCapture")
	pReleaseCapture                = user32.NewProc("ReleaseCapture")
	pGetCursorPos                  = user32.NewProc("GetCursorPos")
	pMessageBoxW                   = user32.NewProc("MessageBoxW")
	pSetWindowTextW                = user32.NewProc("SetWindowTextW")
	pLoadCursorW                   = user32.NewProc("LoadCursorW")
	pSystemParametersInfoW         = user32.NewProc("SystemParametersInfoW")
	pGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	pSetWindowDisplayAffinity      = user32.NewProc("SetWindowDisplayAffinity")
	pCreateIconIndirect            = user32.NewProc("CreateIconIndirect")
	pFillRect                      = user32.NewProc("FillRect")
	pDrawTextW                     = user32.NewProc("DrawTextW")
	pFindWindowW                   = user32.NewProc("FindWindowW")
	pSetFocus                      = user32.NewProc("SetFocus")
	pGetSysColorBrush              = user32.NewProc("GetSysColorBrush")
	pGetDpiForSystem               = user32.NewProc("GetDpiForSystem")
	pSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	pCreateFontW                   = gdi32.NewProc("CreateFontW")
	pCreateSolidBrush              = gdi32.NewProc("CreateSolidBrush")
	pCreateRoundRectRgn            = gdi32.NewProc("CreateRoundRectRgn")
	pEllipse                       = gdi32.NewProc("Ellipse")
	pSetBkMode                     = gdi32.NewProc("SetBkMode")
	pSetTextColor                  = gdi32.NewProc("SetTextColor")
	pBitBlt                        = gdi32.NewProc("BitBlt")
	pGetStockObject                = gdi32.NewProc("GetStockObject")
	pCreateDIBSection              = gdi32.NewProc("CreateDIBSection")
	pCreateBitmap                  = gdi32.NewProc("CreateBitmap")
	pShellExecuteW                 = shell32.NewProc("ShellExecuteW")
	pExtractIconExW                = shell32.NewProc("ExtractIconExW")
	pDrawIconEx                    = user32.NewProc("DrawIconEx")
	pDestroyIcon                   = user32.NewProc("DestroyIcon")
)

const (
	wsPopup, wsVisible, wsChild, wsTabstop   = 0x80000000, 0x10000000, 0x40000000, 0x00010000
	wsCaption, wsSysmenu, wsMinimizebox      = 0x00C00000, 0x00080000, 0x00020000
	wsBorder, wsVscroll                      = 0x00800000, 0x00200000
	wsExTopmost, wsExToolwindow, wsExLayered = 0x8, 0x80, 0x80000
	wsExAppwindow, wsExNoactivate            = 0x40000, 0x08000000
	wsExDlgmodalframe, wsExClientedge        = 0x1, 0x200

	wmDestroy, wmPaint, wmClose, wmEraseBkgnd = 0x2, 0xF, 0x10, 0x14
	wmQueryEndSession, wmEndSession           = 0x11, 0x16
	wmSetFont, wmCommand, wmSysCommand        = 0x30, 0x111, 0x112
	wmTimer, wmHotkey                         = 0x113, 0x312
	wmMouseMove, wmLButtonDown, wmLButtonUp   = 0x200, 0x201, 0x202
	wmRButtonUp, wmSetIcon, wmNull            = 0x205, 0x80, 0x0
	wmShowOverlay                             = 0x8001 // WM_APP+1: a second launch asks us to show up

	swHide, swShowNoActivate, swShowMinNoActive, swShow = 0, 4, 7, 5
	scRestore, scClose                                  = 0xF120, 0xF060

	mfString, mfGrayed, mfChecked, mfPopup, mfSeparator = 0x0, 0x1, 0x8, 0x10, 0x800
	tpmReturnCmd, tpmRightButton, tpmBottomAlign        = 0x100, 0x2, 0x20

	modAlt, modControl, modNorepeat = 0x1, 0x2, 0x4000

	cbAddString, cbSetCurSel, cbGetCurSel = 0x143, 0x14E, 0x147
	cbsDropdownList, cbnSelChange         = 0x3, 1
	esAutoHScroll, enChange, bsDefPush    = 0x80, 0x300, 0x1
	idOK, idCancel                        = 1, 2

	mbOK, mbYesNo, mbIconWarning, mbIconInfo, mbTopmost, mbSetForeground = 0x0, 0x4, 0x30, 0x40, 0x40000, 0x10000
	idYes                                                                = 6
)

type point struct{ X, Y int32 }

type wndclassex struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	Private uint32
}

type paintstruct struct {
	Hdc       uintptr
	Erase     int32
	RcPaint   rect32
	Restore   int32
	IncUpdate int32
	Reserved  [32]byte
}

type iconinfo struct {
	Icon     int32
	XHotspot uint32
	YHotspot uint32
	Mask     uintptr
	Color    uintptr
}

type bitmapinfoheader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

func u16(s string) *uint16     { p, _ := syscall.UTF16PtrFromString(s); return p }
func rgb(r, g, b byte) uintptr { return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16 }
func loword(v uintptr) int     { return int(int16(v & 0xffff)) }
func hiword(v uintptr) int     { return int(int16((v >> 16) & 0xffff)) }

// Spoken languages the server understands (code, name), as in the web app.
var languages = [][2]string{
	{"hi", "Hindi / Hinglish"}, {"ml", "Malayalam"}, {"ta", "Tamil"}, {"te", "Telugu"}, {"kn", "Kannada"},
	{"mr", "Marathi"}, {"bn", "Bengali"}, {"gu", "Gujarati"}, {"pa", "Punjabi"}, {"or", "Odia"},
	{"as", "Assamese"}, {"ur", "Urdu"}, {"ne", "Nepali"}, {"mai", "Maithili"}, {"kok", "Konkani"},
	{"doi", "Dogri"}, {"brx", "Bodo"}, {"ks", "Kashmiri"}, {"mni", "Manipuri"}, {"sa", "Sanskrit"},
	{"sat", "Santali"}, {"sd", "Sindhi"},
}

func languageIndex(code string) int {
	for i, l := range languages {
		if l[0] == code {
			return i
		}
	}
	return 0
}

const maxPeople = 12

func peopleLabel(n int) string {
	if n <= 0 {
		return "Not sure"
	}
	return strconv.Itoa(n)
}

// ---------------------------------------------------------------- state

type phase int

const (
	phIdle phase = iota
	phRecording
	phFinishing
	phUploading
	phResult
)

// menu command ids
const (
	cmdStart = 1 + iota
	cmdStop
	cmdPause
	cmdShotNow
	cmdShotsPause
	cmdShotsPref
	cmdOpenFolder
	cmdOpenWeb
	cmdHide
	cmdQuit
	cmdLangBase    = 100
	cmdPeopleBase  = 200
	cmdAllSound    = 300 // "Sound from" menu: all PC sound
	cmdAppBase     = 301 // ... one of the apps listed
	cmdAppMissing  = 398 // ... the remembered app, not running now
	cmdNoSound     = 399 // ... no PC sound
	cmdMicOff      = 400 // "Microphone" menu
	cmdMicDefault  = 401
	cmdMicBase     = 402
	cmdMicMissing  = 499
	cmdUploadAll   = 599 // "Not uploaded" menu
	cmdUploadBase  = 600 // ... upload recording i
	cmdOpenRecBase = 650 // ... open its folder
	maxPendingMenu = 40
)

type finishForm struct {
	hwnd, title, lang, people, countdown uintptr
	left                                 int  // seconds before uploading with the defaults; -1 = stopped
	ready                                bool // set up: changes from now on are the person's
}

type gui struct {
	cfg      config
	loggedIn bool
	scale    float64
	font     uintptr
	fontBold uintptr
	overlay  uintptr
	taskbar  uintptr
	w, h     int32
	hotkeys  bool
	hidden   bool
	icons    map[uintptr]uintptr // colour -> taskbar icon

	sess         *session
	meetingTitle string // a title guessed from the meeting window
	phase        phase
	resultText   string
	resultColor  uintptr
	resultUntil  time.Time
	note         string // last warning from the recording threads
	blink        bool

	dragging, dragMoved bool
	dragFrom            point
	winFrom             rect32

	fin finishForm

	// sources and recordings offered in the last menu (ids refer into these)
	menuApps    []audioApp
	menuMics    []micDevice
	menuPending []pendingRec
	appIcon     uintptr // icon of the app whose sound is recorded (0 = none)
	iconPath    string

	upMu      sync.Mutex
	upQueue   []string // recording folders to send, in order
	upCurrent string   // the one being sent (or that failed)
	upCount   int      // how many in this batch
	upText    string
	upDone    bool
	upErr     error
	upID      string
}

var g *gui

// MEETREC_INSTANCE (for development): a separate floating recorder with its own lock and
// window names, so a test copy can run next to the installed one.
var instance = os.Getenv("MEETREC_INSTANCE")

// ---------------------------------------------------------------- start

// startGUI runs the floating recorder. A double-click gives us a console window of our
// own: we drop it. From a terminal we start a detached copy so the terminal stays free.
func startGUI() {
	switch consoleCount() {
	case 1:
		pFreeConsole.Call()
	case 0:
	default:
		if err := launchDetached(selfPath(), "gui"); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println("The meetrec recorder is running: look for the floating button (bottom right).")
		return
	}
	setupLog()
	// a downloaded copy installs itself, then the installed copy takes over
	if !runningInstalled() {
		msg, err := install()
		if err != nil {
			messageBox(0, "Couldn't install meetrec: "+err.Error()+"\n\nIt will run from here for now.", mbIconWarning)
		} else if _, statErr := os.Stat(installedExe()); statErr == nil && version != "dev" {
			if msg != "" {
				messageBox(0, msg+"\n\nYou can delete the downloaded file.", mbIconInfo)
			}
			if err := launchDetached(installedExe(), "gui"); err == nil {
				return
			}
		}
	}
	runGUI()
}

func setupLog() {
	dir := filepath.Dir(configPath())
	_ = os.MkdirAll(dir, 0o700)
	if f, err := os.OpenFile(filepath.Join(dir, "meetrec.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		log.SetOutput(f)
		_ = debug.SetCrashOutput(f, debug.CrashOptions{})
	}
	log.Printf("meetrec %s gui starting", version)
}

func messageBox(owner uintptr, text string, flags uintptr) int {
	r, _, _ := pMessageBoxW.Call(owner, uintptr(unsafe.Pointer(u16(text))), uintptr(unsafe.Pointer(u16("meetrec"))),
		flags|mbTopmost|mbSetForeground)
	return int(r)
}

func runGUI() {
	runtime.LockOSThread()
	// one floating recorder at a time: a second launch just brings the first one back
	mutexName := u16("Local\\meetrec-gui" + instance)
	if h, err := windows.CreateMutex(nil, false, mutexName); err == windows.ERROR_ALREADY_EXISTS {
		if hw, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(u16("meetrecOverlay"+instance))), 0); hw != 0 {
			pPostMessageW.Call(hw, wmShowOverlay, 0, 0)
		}
		return
	} else if h != 0 {
		defer windows.CloseHandle(h)
	}
	pSetProcessDpiAwarenessContext.Call(^uintptr(3)) // -4: per-monitor aware v2

	g = &gui{icons: map[uintptr]uintptr{}}
	cfg, err := loadConfig()
	g.cfg, g.loggedIn = cfg, err == nil
	if g.cfg.GUI.Language == "" {
		g.cfg.GUI.Language = "hi"
	}
	dpi, _, _ := pGetDpiForSystem.Call()
	if dpi == 0 {
		dpi = 96
	}
	g.scale = float64(dpi) / 96
	g.font = makeFont(10, 400)
	g.fontBold = makeFont(10, 600)
	g.w, g.h = g.px(196), g.px(40)
	g.createWindows()
	if !g.loggedIn {
		g.note = "not signed in: recordings are kept but can't be sent yet"
	}
	g.loadAppIcon(g.cfg.GUI.AppPath)

	var m msg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		if g.fin.hwnd != 0 {
			if d, _, _ := pIsDialogMessageW.Call(g.fin.hwnd, uintptr(unsafe.Pointer(&m))); d != 0 {
				continue
			}
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (g *gui) px(v float64) int32 { return int32(v*g.scale + 0.5) }

func makeFont(pt int, weight int) uintptr {
	dpi, _, _ := pGetDpiForSystem.Call()
	if dpi == 0 {
		dpi = 96
	}
	h, _, _ := pCreateFontW.Call(uintptr(-int32(pt*int(dpi)/72)), 0, 0, 0, uintptr(weight), 0, 0, 0, 1, 0, 0, 5, 0,
		uintptr(unsafe.Pointer(u16("Segoe UI"))))
	return h
}

func registerClass(name string, proc uintptr, bg uintptr) {
	cursor, _, _ := pLoadCursorW.Call(0, 32512) // IDC_ARROW
	wc := wndclassex{WndProc: proc, Cursor: cursor, Background: bg, ClassName: u16(name)}
	wc.Size = uint32(unsafe.Sizeof(wc))
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
}

func (g *gui) createWindows() {
	registerClass("meetrecOverlay"+instance, windows.NewCallback(overlayProc), 0)
	registerClass("meetrecTaskbar"+instance, windows.NewCallback(taskbarProc), 0)
	btnFace, _, _ := pGetSysColorBrush.Call(15) // COLOR_BTNFACE
	registerClass("meetrecFinish"+instance, windows.NewCallback(finishProc), btnFace)

	x, y := g.cfg.GUI.X, g.cfg.GUI.Y
	if !g.onScreen(x, y) {
		var wa rect32
		pSystemParametersInfoW.Call(0x30, 0, uintptr(unsafe.Pointer(&wa)), 0) // SPI_GETWORKAREA
		x, y = wa.Right-g.w-g.px(24), wa.Bottom-g.h-g.px(24)
	}
	g.overlay, _, _ = pCreateWindowExW.Call(wsExTopmost|wsExToolwindow|wsExLayered|wsExNoactivate,
		uintptr(unsafe.Pointer(u16("meetrecOverlay"+instance))), uintptr(unsafe.Pointer(u16("meetrec"))), wsPopup,
		uintptr(x), uintptr(y), uintptr(g.w), uintptr(g.h), 0, 0, 0, 0)
	pSetLayeredWindowAttributes.Call(g.overlay, 0, 238, 2) // LWA_ALPHA
	rgn, _, _ := pCreateRoundRectRgn.Call(0, 0, uintptr(g.w+1), uintptr(g.h+1), uintptr(g.h), uintptr(g.h))
	pSetWindowRgn.Call(g.overlay, rgn, 1)
	// keep the pill out of screen shares and our own screenshots (Windows 10 2004+)
	if os.Getenv("MEETREC_SHOW_IN_CAPTURE") == "" {
		pSetWindowDisplayAffinity.Call(g.overlay, 0x11)
	}

	// the taskbar button shown while the pill is hidden: an off-screen, minimised window
	g.taskbar, _, _ = pCreateWindowExW.Call(wsExAppwindow, uintptr(unsafe.Pointer(u16("meetrecTaskbar"+instance))),
		uintptr(unsafe.Pointer(u16("meetrec"))), wsCaption|wsSysmenu|wsMinimizebox,
		uintptr(^uint32(31999)), uintptr(^uint32(31999)), 200, 50, 0, 0, 0, 0)
	g.setTaskbarIcon(rgb(120, 120, 120))

	pShowWindow.Call(g.overlay, swShowNoActivate)
	g.registerHotkeys(true)
	pSetTimer.Call(g.overlay, 1, 500, 0)
}

func (g *gui) onScreen(x, y int32) bool {
	if x == 0 && y == 0 {
		return false
	}
	vx, _, _ := pGetSystemMetrics.Call(76)
	vy, _, _ := pGetSystemMetrics.Call(77)
	vw, _, _ := pGetSystemMetrics.Call(78)
	vh, _, _ := pGetSystemMetrics.Call(79)
	return x >= int32(vx) && y >= int32(vy) && x+g.w <= int32(vx)+int32(vw) && y+g.h <= int32(vy)+int32(vh)
}

func (g *gui) savePrefs() {
	cur, _ := loadConfig() // don't overwrite a sign-in done meanwhile
	cur.GUI = g.cfg.GUI
	if err := saveConfig(cur); err != nil {
		log.Printf("saving preferences: %v", err)
	}
	if cur.Server != "" && cur.Token != "" {
		g.cfg, g.loggedIn = cur, true
	}
}

// ---------------------------------------------------------------- hotkeys, show/hide

var hotkeyKeys = []struct {
	id  uintptr
	key uintptr
}{{1, 'R'}, {2, 'P'}, {3, 'S'}, {4, 'X'}}

func (g *gui) registerHotkeys(on bool) {
	if on == g.hotkeys {
		return
	}
	for _, k := range hotkeyKeys {
		if on {
			if r, _, _ := pRegisterHotKey.Call(g.overlay, k.id, modControl|modAlt|modNorepeat, k.key); r == 0 {
				log.Printf("Ctrl+Alt+%c is taken by another program", rune(k.key))
			}
		} else {
			pUnregisterHotKey.Call(g.overlay, k.id)
		}
	}
	g.hotkeys = on
}

func (g *gui) hide() {
	g.hidden = true
	g.registerHotkeys(false)
	pShowWindow.Call(g.overlay, swHide)
	g.updateTaskbar()
	pShowWindow.Call(g.taskbar, swShowMinNoActive)
}

func (g *gui) show() {
	g.hidden = false
	pShowWindow.Call(g.taskbar, swHide)
	pShowWindow.Call(g.overlay, swShowNoActivate)
	pSetWindowPos.Call(g.overlay, ^uintptr(0), 0, 0, 0, 0, 0x1|0x2|0x10) // HWND_TOPMOST, no move/size/activate
	g.registerHotkeys(true)
}

func (g *gui) updateTaskbar() {
	text, color := g.statusText()
	if name := g.soundName(); name != "" {
		text += " · " + name
	}
	pSetWindowTextW.Call(g.taskbar, uintptr(unsafe.Pointer(u16("meetrec – "+text))))
	g.setTaskbarIcon(color)
}

// setTaskbarIcon draws a coloured dot as the window icon (no icon resources needed).
func (g *gui) setTaskbarIcon(color uintptr) {
	icon, ok := g.icons[color]
	if !ok {
		icon = dotIcon(32, byte(color), byte(color>>8), byte(color>>16))
		g.icons[color] = icon
	}
	pSendMessageW.Call(g.taskbar, wmSetIcon, 0, icon)
	pSendMessageW.Call(g.taskbar, wmSetIcon, 1, icon)
}

func dotIcon(size int32, r, gr, b byte) uintptr {
	bi := bitmapinfoheader{Width: size, Height: -size, Planes: 1, BitCount: 32}
	bi.Size = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	color, _, _ := pCreateDIBSection.Call(0, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	px := unsafe.Slice((*uint32)(bits), size*size)
	c := float64(size-1) / 2
	for y := int32(0); y < size; y++ {
		for x := int32(0); x < size; x++ {
			dx, dy := float64(x)-c, float64(y)-c
			d := c - 1 - sqrt(dx*dx+dy*dy) // >0 inside the dot
			a := clamp01(d + 0.5)
			if a > 0 {
				A := uint32(a * 255)
				px[y*size+x] = A<<24 | uint32(float64(r)*a)<<16 | uint32(float64(gr)*a)<<8 | uint32(float64(b)*a)
			}
		}
	}
	mask, _, _ := pCreateBitmap.Call(uintptr(size), uintptr(size), 1, 1, 0)
	ii := iconinfo{Icon: 1, Mask: mask, Color: color}
	icon, _, _ := pCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	return icon
}

func sqrt(v float64) float64 {
	if v <= 0 {
		return 0
	}
	x := v
	for i := 0; i < 20; i++ {
		x = (x + v/x) / 2
	}
	return x
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ---------------------------------------------------------------- overlay

func overlayProc(hwnd, m, wp, lp uintptr) uintptr {
	switch m {
	case wmEraseBkgnd:
		return 1
	case wmPaint:
		var ps paintstruct
		hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		g.draw(hdc)
		pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case 0x318: // WM_PRINTCLIENT: draw into someone else's DC (PrintWindow; the pill is hidden from screen capture)
		g.draw(wp)
		return 0
	case wmTimer:
		g.tick()
		return 0
	case wmHotkey:
		g.hotkey(wp)
		return 0
	case wmLButtonDown:
		var pt point
		pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		g.dragging, g.dragMoved, g.dragFrom = true, false, pt
		pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&g.winFrom)))
		pSetCapture.Call(hwnd)
		return 0
	case wmMouseMove:
		if g.dragging {
			var pt point
			pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
			dx, dy := pt.X-g.dragFrom.X, pt.Y-g.dragFrom.Y
			if g.dragMoved || dx*dx+dy*dy > 16 {
				g.dragMoved = true
				pSetWindowPos.Call(hwnd, 0, uintptr(g.winFrom.Left+dx), uintptr(g.winFrom.Top+dy), 0, 0, 0x1|0x4|0x10)
			}
		}
		return 0
	case wmLButtonUp:
		if g.dragging {
			g.dragging = false
			pReleaseCapture.Call()
			if g.dragMoved {
				var r rect32
				pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
				g.cfg.GUI.X, g.cfg.GUI.Y = r.Left, r.Top
				g.savePrefs()
			} else {
				g.menu()
			}
		}
		return 0
	case wmRButtonUp:
		g.menu()
		return 0
	case wmShowOverlay:
		g.show()
		return 0
	case wmCommand: // menu commands sent by other programs (and tests)
		g.command(loword(wp))
		return 0
	case wmQueryEndSession:
		return 1
	case wmEndSession:
		if wp != 0 {
			g.emergencyStop("Windows is shutting down")
		}
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

func taskbarProc(hwnd, m, wp, lp uintptr) uintptr {
	switch m {
	case wmSysCommand:
		if wp&0xFFF0 == scRestore || wp&0xFFF0 == scClose {
			g.show() // clicking the taskbar button brings the floating pill back
			return 0
		}
	case wmClose:
		g.show()
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

// statusText: what the pill (and the taskbar button) says, and the dot colour.
func (g *gui) statusText() (string, uintptr) {
	switch g.phase {
	case phRecording:
		if g.sess.isPaused() {
			return "Paused " + fmtDur(g.sess.position()), rgb(240, 170, 40)
		}
		return fmtDur(g.sess.position()), rgb(235, 60, 50)
	case phFinishing:
		return "Recorded " + fmtDur(g.sess.position()), rgb(240, 170, 40)
	case phUploading:
		g.upMu.Lock()
		t := g.upText
		g.upMu.Unlock()
		if t == "" {
			t = "Sending…"
		}
		return t, rgb(70, 140, 240)
	case phResult:
		return g.resultText, g.resultColor
	}
	if !g.loggedIn {
		return "Record (not signed in)", rgb(140, 140, 140)
	}
	return "Record", rgb(140, 140, 140)
}

func (g *gui) draw(hdc uintptr) {
	w, h := g.w, g.h
	mem, _, _ := pCreateCompatibleDC.Call(hdc)
	bmp, _, _ := pCreateCompatibleBitmap.Call(hdc, uintptr(w), uintptr(h))
	old, _, _ := pSelectObject.Call(mem, bmp)
	defer func() {
		pSelectObject.Call(mem, old)
		pDeleteObject.Call(bmp)
		pDeleteDC.Call(mem)
	}()

	fill := func(r rect32, c uintptr) {
		br, _, _ := pCreateSolidBrush.Call(c)
		pFillRect.Call(mem, uintptr(unsafe.Pointer(&r)), br)
		pDeleteObject.Call(br)
	}
	fill(rect32{0, 0, w, h}, rgb(32, 33, 38))

	text, color := g.statusText()
	// status dot (blinks while recording)
	if !(g.phase == phRecording && !g.sess.isPaused() && g.blink) {
		br, _, _ := pCreateSolidBrush.Call(color)
		ob, _, _ := pSelectObject.Call(mem, br)
		nullPen, _, _ := pGetStockObject.Call(8) // NULL_PEN
		op, _, _ := pSelectObject.Call(mem, nullPen)
		cx, cy, r := g.px(18), h/2, g.px(6)
		pEllipse.Call(mem, uintptr(cx-r), uintptr(cy-r), uintptr(cx+r+1), uintptr(cy+r+1))
		pSelectObject.Call(mem, op)
		pSelectObject.Call(mem, ob)
		pDeleteObject.Call(br)
	}

	right := w - g.px(12)
	iconSize := g.px(20)
	if g.phase == phRecording {
		// mic and PC level bars
		mic, pc := g.sess.levels()
		bw, bh := g.px(4), g.px(20)
		for i, lvl := range []float32{pc, mic} {
			if lvl < 0 {
				continue
			}
			x := w - g.px(14) - int32(i)*g.px(8) - bw
			top := h/2 - bh/2
			fill(rect32{x, top, x + bw, top + bh}, rgb(60, 62, 70))
			v := float64(lvl) * 2
			if v > 1 {
				v = 1
			}
			lh := int32(float64(bh) * v)
			fill(rect32{x, top + bh - lh, x + bw, top + bh}, rgb(90, 200, 120))
		}
		right = w - g.px(34)
	}
	// whose sound: the app's icon (or its short name when it has none)
	if name := g.soundName(); name != "" {
		if g.appIcon != 0 {
			x := right - iconSize
			pDrawIconEx.Call(mem, uintptr(x), uintptr((h-iconSize)/2), g.appIcon, uintptr(iconSize), uintptr(iconSize), 0, 0, 3)
			right = x - g.px(6)
		} else {
			text += " · " + shorten(name, 8)
		}
	}

	pSetBkMode.Call(mem, 1) // TRANSPARENT
	pSetTextColor.Call(mem, rgb(240, 240, 242))
	f := g.font
	if g.phase == phRecording {
		f = g.fontBold
	}
	of, _, _ := pSelectObject.Call(mem, f)
	tr := rect32{g.px(32), 0, right, h}
	t := u16(text)
	pDrawTextW.Call(mem, uintptr(unsafe.Pointer(t)), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), 0x20|0x4|0x8000)
	pSelectObject.Call(mem, of)

	pBitBlt.Call(hdc, 0, 0, uintptr(w), uintptr(h), mem, 0, 0, 0x00CC0020) // SRCCOPY
}

func (g *gui) redraw() {
	pInvalidateRect.Call(g.overlay, 0, 0)
	if g.hidden {
		g.updateTaskbar()
	}
}

func (g *gui) tick() {
	g.blink = !g.blink
	if g.sess != nil && g.phase == phRecording {
		for _, m := range g.sess.messages() {
			log.Printf("recording: %s", m)
			g.note = m
		}
	}
	if g.phase == phUploading {
		g.upMu.Lock()
		done, err, id := g.upDone, g.upErr, g.upID
		g.upMu.Unlock()
		if done {
			g.upMu.Lock()
			dir, left := g.upCurrent, len(g.upQueue)
			g.upMu.Unlock()
			if err != nil {
				log.Printf("upload of %s failed: %v", dir, err)
				g.result("Not sent: saved", rgb(235, 60, 50), 30*time.Second)
				more := ""
				if left > 0 {
					more = fmt.Sprintf("\n\n%d more waiting weren't tried.", left)
				}
				messageBox(0, fmt.Sprintf("Couldn't send the recording:\n%v\n\nIt's saved in\n%s\n\nTry again later from the menu: Not uploaded.%s",
					err, dir, more), mbIconWarning)
			} else {
				log.Printf("sent %s as meeting %s", dir, id)
				sent := "Sent ✓"
				if g.upCount > 1 {
					sent = fmt.Sprintf("Sent %d ✓", g.upCount)
				}
				g.result(sent, rgb(80, 190, 110), 8*time.Second)
			}
		}
	}
	if g.phase == phResult && time.Now().After(g.resultUntil) {
		g.phase = phIdle
	}
	g.redraw()
}

func (g *gui) result(text string, color uintptr, d time.Duration) {
	g.phase, g.resultText, g.resultColor, g.resultUntil = phResult, text, color, time.Now().Add(d)
	g.redraw()
}

func (g *gui) hotkey(id uintptr) {
	switch id {
	case 1:
		if g.phase == phRecording {
			g.stop()
		} else if g.canStart() {
			g.start()
		}
	case 2:
		if g.phase == phRecording {
			g.sess.togglePause()
		}
	case 3:
		if g.phase == phRecording {
			g.sess.screenshot()
		}
	case 4:
		if g.phase == phRecording {
			g.toggleShots()
		}
	}
	g.redraw()
}

func (g *gui) canStart() bool { return g.phase == phIdle || g.phase == phResult }

// ---------------------------------------------------------------- sources

// soundName: the app whose sound is (or will be) recorded; "" for all PC sound.
func (g *gui) soundName() string {
	if g.sess != nil && (g.phase == phRecording || g.phase == phFinishing) {
		return g.sess.app.Name
	}
	if g.cfg.GUI.App != "" && !g.cfg.GUI.NoSystem {
		return appDisplayName(g.cfg.GUI.App)
	}
	return ""
}

func (g *gui) loadAppIcon(path string) {
	if path == g.iconPath {
		return
	}
	if g.appIcon != 0 {
		pDestroyIcon.Call(g.appIcon)
		g.appIcon = 0
	}
	g.iconPath = path
	if path == "" {
		return
	}
	var large uintptr
	if n, _, _ := pExtractIconExW.Call(uintptr(unsafe.Pointer(u16(path))), 0, uintptr(unsafe.Pointer(&large)), 0, 1); n > 0 && large != 0 {
		g.appIcon = large
	}
}

func (g *gui) soundMenu(add func(m uintptr, flags uintptr, id int, text string), check func(bool) uintptr, grayed uintptr) (uintptr, string) {
	m, _, _ := pCreatePopupMenu.Call()
	prefs := g.cfg.GUI
	add(m, grayed|check(prefs.App == "" && !prefs.NoSystem), cmdAllSound, "All PC sound")
	pAppendMenuW.Call(m, mfSeparator, 0, 0)
	supported := appCaptureSupported()
	appFlags := grayed
	if !supported {
		appFlags = mfGrayed
		add(m, mfGrayed, 0, "Only one app: needs Windows 11")
	}
	g.menuApps = nil
	if grayed == 0 {
		apps, err := listAudioApps()
		if err != nil {
			log.Printf("listing apps: %v", err)
		}
		self := strings.ToLower(filepath.Base(selfPath()))
		for _, a := range apps {
			if a.Exe != self && len(g.menuApps) < 20 {
				g.menuApps = append(g.menuApps, a)
			}
		}
	}
	listed := false
	for i, a := range g.menuApps {
		on := !prefs.NoSystem && strings.EqualFold(a.Exe, prefs.App)
		listed = listed || on
		add(m, appFlags|check(on), cmdAppBase+i, "Only "+a.Name)
	}
	if prefs.App != "" && !prefs.NoSystem && !listed {
		label := "Only " + appDisplayName(prefs.App)
		if grayed == 0 {
			label += " (not playing now)"
		}
		add(m, appFlags|mfChecked, cmdAppMissing, label)
	}
	if len(g.menuApps) == 0 && grayed == 0 && supported {
		add(m, mfGrayed, 0, "(apps show up here once they play sound)")
	}
	pAppendMenuW.Call(m, mfSeparator, 0, 0)
	add(m, grayed|check(prefs.NoSystem), cmdNoSound, "No PC sound (in-person meeting)")
	label := "All PC sound"
	switch {
	case prefs.NoSystem:
		label = "none"
	case prefs.App != "":
		label = appDisplayName(prefs.App)
	}
	return m, "Sound from: " + label
}

func (g *gui) micMenu(add func(m uintptr, flags uintptr, id int, text string), check func(bool) uintptr, grayed uintptr) (uintptr, string) {
	m, _, _ := pCreatePopupMenu.Call()
	prefs := g.cfg.GUI
	g.menuMics = nil
	defName := ""
	if grayed == 0 {
		var err error
		if g.menuMics, defName, err = listMics(); err != nil {
			log.Printf("listing microphones: %v", err)
		}
	}
	add(m, grayed|check(prefs.NoMic), cmdMicOff, "Off")
	def := "Windows default"
	if defName != "" {
		def += " (" + shorten(defName, 40) + ")"
	}
	add(m, grayed|check(!prefs.NoMic && prefs.MicID == ""), cmdMicDefault, def)
	pAppendMenuW.Call(m, mfSeparator, 0, 0)
	found := false
	for i, d := range g.menuMics {
		on := !prefs.NoMic && d.ID == prefs.MicID
		found = found || on
		add(m, grayed|check(on), cmdMicBase+i, shorten(d.Name, 60))
	}
	if prefs.MicID != "" && !prefs.NoMic && !found {
		label := shorten(prefs.MicName, 50)
		if grayed == 0 {
			label += " (unplugged)"
		}
		add(m, grayed|mfChecked, cmdMicMissing, label)
	}
	label := "default"
	switch {
	case prefs.NoMic:
		label = "off"
	case prefs.MicID != "":
		label = shorten(prefs.MicName, 24)
	}
	return m, "Microphone: " + label
}

// pendingMenu: "Not uploaded (N)" → one entry per recording (Upload / Open folder), then
// "Upload all". 0 when there's nothing waiting.
func (g *gui) pendingMenu(add func(m uintptr, flags uintptr, id int, text string)) (uintptr, string) {
	skip := ""
	if g.sess != nil && g.phase == phRecording {
		skip = g.sess.dir
	}
	g.menuPending = pendingRecordings(defaultBaseDir(), skip)
	if len(g.menuPending) == 0 {
		return 0, ""
	}
	if len(g.menuPending) > maxPendingMenu {
		g.menuPending = g.menuPending[:maxPendingMenu]
	}
	var busy uintptr // one upload at a time, and not while recording
	if !g.canStart() {
		busy = mfGrayed
	}
	m, _, _ := pCreatePopupMenu.Call()
	if busy != 0 {
		add(m, mfGrayed, 0, "(can be sent once the current recording is done)")
	}
	for i, r := range g.menuPending {
		item, _, _ := pCreatePopupMenu.Call()
		add(item, busy, cmdUploadBase+i, "Upload")
		add(item, 0, cmdOpenRecBase+i, "Open folder")
		pAppendMenuW.Call(m, mfPopup, item, uintptr(unsafe.Pointer(u16(r.label()))))
	}
	pAppendMenuW.Call(m, mfSeparator, 0, 0)
	all := "Upload all"
	if len(g.menuPending) > 1 {
		all = fmt.Sprintf("Upload all %d", len(g.menuPending))
	}
	add(m, busy, cmdUploadAll, all)
	if !g.loggedIn {
		add(m, mfGrayed, 0, "(sign in first: meetrec login in a terminal)")
	}
	return m, fmt.Sprintf("Not uploaded (%d)", len(g.menuPending))
}

// upload sends recording folders one after another, with their saved meeting.json.
func (g *gui) upload(dirs []string) {
	if len(dirs) == 0 {
		return
	}
	if !g.loggedIn {
		messageBox(g.overlay, "meetrec isn't signed in yet. Sign in once in a terminal (the command is in the web app: "+
			"Settings → Desktop recorder), then send the recordings from the menu: Not uploaded.", mbIconInfo)
		return
	}
	g.phase = phUploading
	g.upMu.Lock()
	g.upQueue, g.upCount = append([]string(nil), dirs...), len(dirs)
	g.upText, g.upDone, g.upErr, g.upID, g.upCurrent = "Sending…", false, nil, "", ""
	g.upMu.Unlock()
	cfg := g.cfg
	go func() {
		var id string
		var err error
		for n := 1; ; n++ {
			g.upMu.Lock()
			if len(g.upQueue) == 0 {
				g.upMu.Unlock()
				break
			}
			dir := g.upQueue[0]
			g.upQueue = g.upQueue[1:]
			g.upCurrent = dir
			g.upMu.Unlock()
			prefix := ""
			if len(dirs) > 1 {
				prefix = fmt.Sprintf("%d/%d ", n, len(dirs))
			}
			var fields finishFields
			if fields, err = readFields(dir); err != nil {
				break
			}
			id, err = newClient(cfg).uploadRecording(dir, fields, func(s string) {
				t := "Sending…"
				if m := percent.FindStringSubmatch(s); m != nil {
					t = "Sending " + m[1] + "%"
				} else if m := shotCount.FindStringSubmatch(s); m != nil {
					t = "Shots " + m[1]
				} else if strings.Contains(s, "retrying") {
					t = "Retrying…"
				}
				g.upMu.Lock()
				g.upText = prefix + t
				g.upMu.Unlock()
			})
			if err != nil {
				break
			}
			log.Printf("sent %s as meeting %s", dir, id)
		}
		g.upMu.Lock()
		g.upDone, g.upErr, g.upID = true, err, id
		g.upMu.Unlock()
	}()
	g.redraw()
}

// ---------------------------------------------------------------- menu

func (g *gui) menu() {
	menu, _, _ := pCreatePopupMenu.Call()
	defer pDestroyMenu.Call(menu)
	add := func(m uintptr, flags uintptr, id int, text string) {
		pAppendMenuW.Call(m, flags, uintptr(id), uintptr(unsafe.Pointer(u16(text))))
	}
	sep := func() { pAppendMenuW.Call(menu, mfSeparator, 0, 0) }
	check := func(on bool) uintptr {
		if on {
			return mfChecked
		}
		return 0
	}

	if g.note != "" {
		add(menu, mfGrayed, 0, "⚠ "+shorten(g.note, 70))
		sep()
	}
	switch g.phase {
	case phRecording:
		add(menu, mfString, cmdStop, "Stop…\tCtrl+Alt+R")
		pause := "Pause\tCtrl+Alt+P"
		if g.sess.isPaused() {
			pause = "Resume\tCtrl+Alt+P"
		}
		add(menu, mfString, cmdPause, pause)
		if g.sess.shooter != nil {
			add(menu, mfString, cmdShotNow, "Screenshot now\tCtrl+Alt+S")
			add(menu, mfString|check(g.sess.shooter.isPaused()), cmdShotsPause, "Pause screenshots\tCtrl+Alt+X")
		}
	case phFinishing:
		add(menu, mfGrayed, 0, "Choose title and language in the open window")
	case phUploading:
		add(menu, mfGrayed, 0, "Sending the recording…")
	default:
		add(menu, mfString, cmdStart, "Start recording\tCtrl+Alt+R")
	}
	if sub, label := g.pendingMenu(add); sub != 0 {
		pAppendMenuW.Call(menu, mfPopup, sub, uintptr(unsafe.Pointer(u16(label))))
	}
	sep()
	langs, _, _ := pCreatePopupMenu.Call()
	for i, l := range languages {
		add(langs, mfString|check(l[0] == g.cfg.GUI.Language), cmdLangBase+i, l[1])
	}
	pAppendMenuW.Call(menu, mfPopup, langs, uintptr(unsafe.Pointer(u16("Language: "+languages[languageIndex(g.cfg.GUI.Language)][1]))))
	people, _, _ := pCreatePopupMenu.Call()
	for n := 0; n <= maxPeople; n++ {
		if n == 1 {
			continue
		}
		add(people, mfString|check(n == g.cfg.GUI.People), cmdPeopleBase+n, peopleLabel(n))
	}
	pAppendMenuW.Call(menu, mfPopup, people, uintptr(unsafe.Pointer(u16("People: "+peopleLabel(g.cfg.GUI.People)))))
	// where the sound comes from: chosen before recording, shown (greyed) during it
	var fixed uintptr
	if g.phase == phRecording || g.phase == phFinishing || g.phase == phUploading {
		fixed = mfGrayed
	}
	sound, soundLabel := g.soundMenu(add, check, fixed)
	pAppendMenuW.Call(menu, mfPopup|fixed, sound, uintptr(unsafe.Pointer(u16(soundLabel))))
	mics, micLabel := g.micMenu(add, check, fixed)
	pAppendMenuW.Call(menu, mfPopup|fixed, mics, uintptr(unsafe.Pointer(u16(micLabel))))
	if g.phase != phRecording {
		add(menu, mfString|check(!g.cfg.GUI.NoShots), cmdShotsPref, "Take slide screenshots")
	}
	sep()
	add(menu, mfString, cmdOpenFolder, "Open recordings folder")
	if g.loggedIn {
		add(menu, mfString, cmdOpenWeb, "Open Meeting Notes")
	}
	add(menu, mfString, cmdHide, "Hide to taskbar")
	add(menu, mfString, cmdQuit, "Quit")

	var r rect32
	pGetWindowRect.Call(g.overlay, uintptr(unsafe.Pointer(&r)))
	pSetForegroundWindow.Call(g.overlay)
	id, _, _ := pTrackPopupMenu.Call(menu, tpmReturnCmd|tpmRightButton|tpmBottomAlign, uintptr(r.Left), uintptr(r.Top), 0, g.overlay, 0)
	pPostMessageW.Call(g.overlay, wmNull, 0, 0)
	g.command(int(id))
}

func (g *gui) command(id int) {
	switch {
	case id == cmdStart:
		if g.canStart() {
			g.start()
		}
	case id == cmdStop:
		g.stop()
	case id == cmdPause:
		g.sess.togglePause()
	case id == cmdShotNow:
		g.sess.screenshot()
	case id == cmdShotsPause:
		g.toggleShots()
	case id == cmdShotsPref:
		g.cfg.GUI.NoShots = !g.cfg.GUI.NoShots
		g.savePrefs()
	case id == cmdOpenFolder:
		dir := defaultBaseDir()
		if g.sess != nil {
			dir = g.sess.dir
		}
		_ = os.MkdirAll(dir, 0o755)
		_ = exec.Command("explorer", dir).Start()
	case id == cmdOpenWeb:
		openURL(g.cfg.Server)
	case id == cmdHide:
		g.hide()
	case id == cmdQuit:
		g.quit()
	case id >= cmdLangBase && id < cmdLangBase+len(languages):
		g.cfg.GUI.Language = languages[id-cmdLangBase][0]
		g.savePrefs()
	case id >= cmdPeopleBase && id <= cmdPeopleBase+maxPeople:
		g.cfg.GUI.People = id - cmdPeopleBase
		g.savePrefs()
	case id == cmdAllSound:
		g.cfg.GUI.App, g.cfg.GUI.AppPath, g.cfg.GUI.NoSystem = "", "", false
		g.loadAppIcon("")
		g.savePrefs()
	case id >= cmdAppBase && id < cmdAppBase+len(g.menuApps):
		a := g.menuApps[id-cmdAppBase]
		g.cfg.GUI.App, g.cfg.GUI.AppPath, g.cfg.GUI.NoSystem = a.Exe, a.Path, false
		g.loadAppIcon(a.Path)
		g.savePrefs()
	case id == cmdNoSound:
		g.cfg.GUI.NoSystem = true
		g.cfg.GUI.NoMic = false // something has to be recorded
		g.savePrefs()
	case id == cmdMicOff:
		g.cfg.GUI.NoMic = true
		g.cfg.GUI.NoSystem = false
		g.savePrefs()
	case id == cmdMicDefault:
		g.cfg.GUI.NoMic, g.cfg.GUI.MicID, g.cfg.GUI.MicName = false, "", ""
		g.savePrefs()
	case id == cmdUploadAll && g.canStart():
		var dirs []string
		for i := len(g.menuPending) - 1; i >= 0; i-- { // oldest first
			dirs = append(dirs, g.menuPending[i].Dir)
		}
		g.upload(dirs)
	case id >= cmdUploadBase && id < cmdUploadBase+len(g.menuPending) && g.canStart():
		g.upload([]string{g.menuPending[id-cmdUploadBase].Dir})
	case id >= cmdOpenRecBase && id < cmdOpenRecBase+len(g.menuPending):
		_ = exec.Command("explorer", g.menuPending[id-cmdOpenRecBase].Dir).Start()
	case id >= cmdMicBase && id < cmdMicBase+len(g.menuMics):
		d := g.menuMics[id-cmdMicBase]
		g.cfg.GUI.NoMic, g.cfg.GUI.MicID, g.cfg.GUI.MicName = false, d.ID, d.Name
		g.savePrefs()
	}
	g.redraw()
}

func openURL(u string) {
	pShellExecuteW.Call(0, uintptr(unsafe.Pointer(u16("open"))), uintptr(unsafe.Pointer(u16(u))), 0, 0, swShow)
}

func (g *gui) toggleShots() {
	if paused, ok := g.sess.toggleShots(); ok {
		g.note = map[bool]string{true: "screenshots paused", false: ""}[paused]
	}
}

// ---------------------------------------------------------------- record / stop / finish

func (g *gui) start() {
	p := g.cfg.GUI
	sess, err := startSession(recOptions{BaseDir: defaultBaseDir(), Shots: !p.NoShots, Window: "auto",
		NoMic: p.NoMic, MicDevice: p.MicID, NoSystem: p.NoSystem, App: p.App})
	if err != nil {
		messageBox(g.overlay, "Couldn't start recording: "+err.Error(), mbIconWarning)
		return
	}
	g.sess, g.phase, g.note = sess, phRecording, ""
	if !g.loggedIn {
		g.note = "not signed in: this recording will be kept but not sent"
	}
	// the icon shows whose sound is recorded (none when it fell back to all PC sound)
	g.loadAppIcon(sess.app.Path)
	if sess.app.Path != "" && sess.app.Path != p.AppPath {
		g.cfg.GUI.AppPath = sess.app.Path
		g.savePrefs()
	}
	g.meetingTitle = guessTitle()
	// a usable meeting.json from the start, in case the PC dies mid-meeting
	_ = sess.writeFields(g.defaultFields())
	log.Printf("recording to %s", sess.dir)
	g.redraw()
}

func (g *gui) defaultFields() finishFields {
	title := g.meetingTitle
	if title == "" {
		title = "Online meeting " + g.sess.started.Format("2 Jan 15:04")
	}
	return finishFields{Title: title, Language: g.cfg.GUI.Language, NotesLanguage: "English", NumSpeakers: g.cfg.GUI.People}
}

func (g *gui) stop() {
	if g.phase != phRecording {
		return
	}
	dur, err := g.sess.stop()
	if err != nil {
		log.Printf("closing the recording: %v", err)
	}
	if t := guessTitle(); g.meetingTitle == "" && t != "" {
		g.meetingTitle = t
	}
	if dur < 3 {
		_ = os.RemoveAll(g.sess.dir)
		g.result("Too short, not kept", rgb(140, 140, 140), 5*time.Second)
		return
	}
	g.phase = phFinishing
	if g.hidden {
		g.show()
	}
	g.openFinish(dur)
}

// emergencyStop: Windows is shutting down (or we're quitting) mid-recording. Close the
// file properly and leave it ready for `meetrec upload`.
func (g *gui) emergencyStop(why string) {
	if g.phase == phRecording {
		_, _ = g.sess.stop()
		_ = g.sess.writeFields(g.defaultFields())
		log.Printf("%s: recording saved in %s (not sent)", why, g.sess.dir)
	}
}

func (g *gui) quit() {
	switch g.phase {
	case phRecording:
		if messageBox(g.overlay, "Stop recording and quit?\n\nThe recording is kept in its folder but not sent.", mbYesNo|mbIconWarning) != idYes {
			return
		}
		g.emergencyStop("quit")
	case phUploading:
		if messageBox(g.overlay, "A recording is still being sent. Quit anyway?\n\nIt's kept, and you can send it later from the menu: Not uploaded.", mbYesNo|mbIconWarning) != idYes {
			return
		}
	case phFinishing:
		g.finish(false)
	}
	g.registerHotkeys(false)
	pDestroyWindow.Call(g.taskbar)
	pDestroyWindow.Call(g.overlay)
}

// guessTitle takes a meeting name from the meeting app's window title when it has one
// ("Weekly sync | Microsoft Teams"); Zoom and Meet titles say nothing useful.
func guessTitle() string {
	w, ok := findWindow("auto")
	if !ok {
		return ""
	}
	t := w.title
	for _, suffix := range []string{" | Microsoft Teams", " - Microsoft Teams", " | Webex", " - Webex"} {
		if i := strings.Index(t, suffix); i > 0 {
			t = strings.TrimSpace(t[:i])
			if strings.EqualFold(t, "Meeting") || strings.Contains(strings.ToLower(t), "microsoft teams") {
				return ""
			}
			t = strings.TrimPrefix(t, "Meeting in ")
			t = strings.TrimPrefix(t, "Meeting with ")
			return shorten(t, 80)
		}
	}
	return ""
}

var percent = regexp.MustCompile(`(\d+)%`)
var shotCount = regexp.MustCompile(`(\d+/\d+)`)

func (g *gui) finish(send bool) {
	f := g.defaultFields()
	if g.fin.hwnd != 0 {
		if t := strings.TrimSpace(getText(g.fin.title)); t != "" {
			f.Title = t
		}
		if i, _, _ := pSendMessageW.Call(g.fin.lang, cbGetCurSel, 0, 0); int(i) >= 0 && int(i) < len(languages) {
			f.Language = languages[i][0]
		}
		if i, _, _ := pSendMessageW.Call(g.fin.people, cbGetCurSel, 0, 0); int(i) >= 0 {
			f.NumSpeakers = peopleFromIndex(int(i))
		}
		h := g.fin.hwnd
		g.fin = finishForm{}
		pDestroyWindow.Call(h)
	}
	g.cfg.GUI.Language, g.cfg.GUI.People = f.Language, f.NumSpeakers
	g.savePrefs()
	g.sess.retitle(f.Title)
	if err := g.sess.writeFields(f); err != nil {
		log.Printf("meeting.json: %v", err)
	}
	f.ChannelLayout = g.sess.channelLayout
	if !send {
		log.Printf("kept %s without sending", g.sess.dir)
		g.result("Saved, not sent", rgb(140, 140, 140), 8*time.Second)
		return
	}
	if !g.loggedIn {
		g.result("Saved, not sent", rgb(240, 170, 40), 30*time.Second)
		messageBox(g.overlay, "meetrec isn't signed in yet, so the recording was saved but not sent:\n"+g.sess.dir+
			"\n\nSign in once in a terminal (the command is in the web app: Settings → Desktop recorder), then send it from "+
			"the menu: Not uploaded.", mbIconInfo)
		return
	}
	g.upload([]string{g.sess.dir})
}

// people combo: index 0 = "Not sure", then 2, 3, ... maxPeople
func peopleFromIndex(i int) int {
	if i <= 0 {
		return 0
	}
	return i + 1
}

func peopleToIndex(n int) int {
	if n <= 1 {
		return 0
	}
	return n - 1
}

func getText(hwnd uintptr) string {
	n, _, _ := pGetWindowTextLengthW.Call(hwnd)
	buf := make([]uint16, n+1)
	pGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return syscall.UTF16ToString(buf)
}

// ---------------------------------------------------------------- the finish window

const autoSendSeconds = 60

func (g *gui) openFinish(dur float64) {
	W, H := g.px(420), g.px(292)
	var wa rect32
	pSystemParametersInfoW.Call(0x30, 0, uintptr(unsafe.Pointer(&wa)), 0)
	x, y := (wa.Left+wa.Right-W)/2, (wa.Top+wa.Bottom-H)/2
	hw, _, _ := pCreateWindowExW.Call(wsExTopmost|wsExDlgmodalframe|wsExAppwindow,
		uintptr(unsafe.Pointer(u16("meetrecFinish"+instance))), uintptr(unsafe.Pointer(u16("meetrec – save the recording"))),
		wsCaption|wsSysmenu, uintptr(x), uintptr(y), uintptr(W), uintptr(H), 0, 0, 0, 0)
	g.fin = finishForm{hwnd: hw, left: autoSendSeconds}
	if !g.loggedIn {
		g.fin.left = -1 // nothing to send to: don't count down
	}

	child := func(class, text string, style uintptr, x, y, w, h float64, id int) uintptr {
		c, _, _ := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(u16(class))), uintptr(unsafe.Pointer(u16(text))),
			wsChild|wsVisible|style, uintptr(g.px(x)), uintptr(g.px(y)), uintptr(g.px(w)), uintptr(g.px(h)), hw, uintptr(id), 0, 0)
		pSendMessageW.Call(c, wmSetFont, g.font, 1)
		return c
	}
	shots := ""
	if n := g.sess.shotsTaken(); n > 0 {
		shots = fmt.Sprintf(" · %d screenshot(s)", n)
	}
	child("STATIC", "Recorded "+fmtDur(dur)+shots, 0, 16, 12, 380, 20, 0)
	child("STATIC", "Title", 0, 16, 44, 90, 20, 0)
	g.fin.title = child("EDIT", g.defaultFields().Title, wsBorder|wsTabstop|esAutoHScroll, 110, 41, 286, 24, 10)
	child("STATIC", "Language", 0, 16, 78, 90, 20, 0)
	g.fin.lang = child("COMBOBOX", "", wsTabstop|wsVscroll|cbsDropdownList, 110, 75, 286, 300, 11)
	for _, l := range languages {
		pSendMessageW.Call(g.fin.lang, cbAddString, 0, uintptr(unsafe.Pointer(u16(l[1]))))
	}
	pSendMessageW.Call(g.fin.lang, cbSetCurSel, uintptr(languageIndex(g.cfg.GUI.Language)), 0)
	child("STATIC", "People", 0, 16, 112, 90, 20, 0)
	g.fin.people = child("COMBOBOX", "", wsTabstop|wsVscroll|cbsDropdownList, 110, 109, 120, 300, 12)
	for n := 0; n <= maxPeople; n++ {
		if n != 1 {
			pSendMessageW.Call(g.fin.people, cbAddString, 0, uintptr(unsafe.Pointer(u16(peopleLabel(n)))))
		}
	}
	pSendMessageW.Call(g.fin.people, cbSetCurSel, uintptr(peopleToIndex(g.cfg.GUI.People)), 0)
	g.fin.countdown = child("STATIC", "", 0, 16, 148, 380, 20, 0)
	child("STATIC", "Discard keeps the recording in its folder without sending it.", 0, 16, 172, 380, 20, 0)
	send := "Upload"
	if !g.loggedIn {
		send = "Save"
	}
	child("BUTTON", send, wsTabstop|bsDefPush, 196, 206, 96, 30, idOK)
	child("BUTTON", "Discard", wsTabstop, 300, 206, 96, 30, idCancel)
	g.updateCountdown()

	pShowWindow.Call(hw, swShow)
	pSetForegroundWindow.Call(hw)
	pSetFocus.Call(g.fin.title)
	pSendMessageW.Call(g.fin.title, 0xB1, 0, ^uintptr(0)) // EM_SETSEL: select all, typing replaces it
	pSetTimer.Call(hw, 2, 1000, 0)
	g.fin.ready = true
}

func (g *gui) updateCountdown() {
	t := ""
	if g.fin.left >= 0 {
		t = fmt.Sprintf("Uploading with these settings in %d s…", g.fin.left)
	}
	pSetWindowTextW.Call(g.fin.countdown, uintptr(unsafe.Pointer(u16(t))))
}

func finishProc(hwnd, m, wp, lp uintptr) uintptr {
	switch m {
	case wmTimer:
		if g.fin.hwnd == hwnd && g.fin.left > 0 {
			g.fin.left--
			g.updateCountdown()
			if g.fin.left == 0 {
				pKillTimer.Call(hwnd, 2)
				g.finish(true)
			}
		}
		return 0
	case wmCommand:
		id, code := loword(wp), hiword(wp)
		switch {
		case id == idOK:
			g.finish(true)
		case id == idCancel:
			g.finish(false)
		case (id == 10 && code == enChange) || ((id == 11 || id == 12) && code == cbnSelChange):
			if g.fin.ready && g.fin.left > 0 { // they're choosing: no automatic upload
				g.fin.left = -1
				g.updateCountdown()
			}
		}
		return 0
	case wmClose:
		g.finish(false)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}
