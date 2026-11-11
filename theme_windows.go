//go:build windows

package main

// The dark look of meetrec's windows, matching the web app: dark panels, light text and the
// accent colour on the main button (orange for work, green for personal). Off in Windows'
// high contrast mode: then everything keeps the system colours.

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	uxtheme = windows.NewLazySystemDLL("uxtheme.dll")
	dwmapi  = windows.NewLazySystemDLL("dwmapi.dll")

	pSetWindowTheme        = uxtheme.NewProc("SetWindowTheme")
	pDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
	pSetBkColor            = gdi32.NewProc("SetBkColor")
	pRoundRect             = gdi32.NewProc("RoundRect")
	pCreatePen             = gdi32.NewProc("CreatePen")
	pGetDlgCtrlID          = user32.NewProc("GetDlgCtrlID")
	pDrawFocusRect         = user32.NewProc("DrawFocusRect")
)

const (
	wmCtlColorEdit, wmCtlColorListbox = 0x133, 0x134
	wmCtlColorBtn, wmCtlColorStatic   = 0x135, 0x138
	wmDrawItem                        = 0x2B
	bsOwnerDraw                       = 0xB
	odsSelected, odsFocus             = 0x1, 0x10
	ssNotify, ssRight                 = 0x100, 0x2
)

// The web app's dark theme.
var dark = struct{ panel, field, text, muted, border, button uintptr }{
	panel: rgb(27, 28, 32), field: rgb(36, 38, 44), text: rgb(235, 233, 229),
	muted: rgb(150, 152, 160), border: rgb(58, 61, 68), button: rgb(48, 50, 58),
}

var (
	accentWork     = rgb(194, 65, 12)
	accentPersonal = rgb(21, 128, 61)
)

type drawitem struct {
	CtlType, CtlID, ItemID, ItemAction, ItemState uint32
	Hwnd, HDC                                     uintptr
	Rc                                            rect32
	ItemData                                      uintptr
}

// highContrast: Windows' high contrast mode is on.
func highContrast() bool {
	hc := struct {
		Size, Flags uint32
		Scheme      uintptr
	}{}
	hc.Size = uint32(unsafe.Sizeof(hc))
	pSystemParametersInfoW.Call(0x42, uintptr(hc.Size), uintptr(unsafe.Pointer(&hc)), 0) // SPI_GETHIGHCONTRAST
	return hc.Flags&1 != 0                                                               // HCF_HIGHCONTRASTON
}

var brushes = map[uintptr]uintptr{}

// brush: a solid brush of this colour, made once and kept.
func brush(c uintptr) uintptr {
	if b, ok := brushes[c]; ok {
		return b
	}
	b, _, _ := pCreateSolidBrush.Call(c)
	brushes[c] = b
	return b
}

// darkTitleBar asks Windows for a dark caption (Windows 10 20H1+ and 11; older ones ignore it).
func darkTitleBar(hwnd uintptr) {
	on := int32(1)
	for _, attr := range []uintptr{20, 19} { // DWMWA_USE_IMMERSIVE_DARK_MODE, and its number before 20H1
		if r, _, _ := pDwmSetWindowAttribute.Call(hwnd, attr, uintptr(unsafe.Pointer(&on)), 4); r == 0 {
			return
		}
	}
}

// darkControl gives an edit or combo box Windows' own dark style (Windows 10 1809+).
func darkControl(hwnd uintptr) {
	pSetWindowTheme.Call(hwnd, uintptr(unsafe.Pointer(u16("DarkMode_CFD"))), 0)
}

// darkMenus makes popup menus dark with uxtheme's SetPreferredAppMode (ordinal 135, not
// documented, Windows 10 1903+; on 1809 the same ordinal only allows dark menus). Where it's
// missing, menus simply stay light.
func darkMenus() {
	h, err := windows.LoadLibrary("uxtheme.dll")
	if err != nil {
		return
	}
	if p, err := windows.GetProcAddressByOrdinal(h, 135); err == nil {
		syscall.SyscallN(p, 2) // ForceDark
		if flush, err := windows.GetProcAddressByOrdinal(h, 136); err == nil {
			syscall.SyscallN(flush) // FlushMenuThemes
		}
	}
}

// ctlColor answers WM_CTLCOLOR*: the colours of the finish window's controls.
func (g *gui) ctlColor(m, hdc, ctl uintptr) uintptr {
	bg, fg := dark.panel, dark.text
	if m == wmCtlColorEdit || m == wmCtlColorListbox {
		bg = dark.field
	}
	switch id, _, _ := pGetDlgCtrlID.Call(ctl); id {
	case idCountdown, idNote:
		fg = dark.muted
	case idBadge:
		fg = personalGreen
	}
	pSetTextColor.Call(hdc, fg)
	pSetBkColor.Call(hdc, bg)
	return brush(bg)
}

// drawButton paints the finish window's buttons: the accent colour for the main one.
func (g *gui) drawButton(di *drawitem) {
	fill, edge, text, font := dark.button, dark.border, dark.text, g.font
	if di.CtlID == idOK {
		fill, text, font = accentWork, rgb(255, 255, 255), g.fontBold
		if g.fin.personalOn {
			fill = accentPersonal
		}
		edge = fill
	}
	if di.ItemState&odsSelected != 0 { // pressed
		fill = shade(fill, 0.8)
	}
	hdc, r := di.HDC, di.Rc
	pFillRect.Call(hdc, uintptr(unsafe.Pointer(&r)), brush(dark.panel)) // behind the round corners
	pen, _, _ := pCreatePen.Call(0, 1, edge)
	oldPen, _, _ := pSelectObject.Call(hdc, pen)
	oldBrush, _, _ := pSelectObject.Call(hdc, brush(fill))
	radius := uintptr(g.px(8))
	pRoundRect.Call(hdc, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right), uintptr(r.Bottom), radius, radius)
	pSelectObject.Call(hdc, oldBrush)
	pSelectObject.Call(hdc, oldPen)
	pDeleteObject.Call(pen)

	pSetBkMode.Call(hdc, 1) // TRANSPARENT
	pSetTextColor.Call(hdc, text)
	oldFont, _, _ := pSelectObject.Call(hdc, font)
	label := u16(getText(di.Hwnd))
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(label)), ^uintptr(0), uintptr(unsafe.Pointer(&r)), 0x1|0x4|0x20) // centred, one line
	pSelectObject.Call(hdc, oldFont)
	if di.ItemState&odsFocus != 0 {
		in := rect32{r.Left + g.px(3), r.Top + g.px(3), r.Right - g.px(3), r.Bottom - g.px(3)}
		pDrawFocusRect.Call(hdc, uintptr(unsafe.Pointer(&in)))
	}
}

// shade darkens a colour (f < 1).
func shade(c uintptr, f float64) uintptr {
	return rgb(byte(float64(c&0xff)*f), byte(float64(c>>8&0xff)*f), byte(float64(c>>16&0xff)*f))
}

// paintPanel fills the finish window's background.
func paintPanel(hwnd, hdc uintptr) {
	var r rect32
	pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	pFillRect.Call(hdc, uintptr(unsafe.Pointer(&r)), brush(dark.panel))
}
