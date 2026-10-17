//go:build windows

package main

// Audio capture with WASAPI (pure Go via go-wca, no cgo):
//   - "PC sound" = loopback of the default output device: whatever the PC plays,
//     i.e. the other people in Zoom / Meet / Teams / any app; or, when an app is
//     chosen, only that app (see sources_windows.go).
//   - "mic"      = the default recording device (or a chosen one): you.
// Each source runs on its own OS thread (COM apartments are per thread). Default
// devices are followed, so switching to headphones mid-meeting keeps working.

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
)

type sourceKind int

const (
	srcMic sourceKind = iota
	srcSystem
)

// audioSource says what one capture thread records.
type audioSource struct {
	kind     sourceKind
	deviceID string   // mic: a chosen device ("" = the Windows default)
	app      audioApp // PC sound: only this app ("" Exe = everything the PC plays)
}

func (s audioSource) String() string {
	switch {
	case s.kind == srcMic:
		return "mic"
	case s.app.Exe != "":
		return s.app.Name + " sound"
	}
	return "PC sound"
}

// captureSource streams mono float32 samples at TargetRate into sink until ctx ends.
// It reopens the device when it disappears or the Windows default changes.
func captureSource(ctx context.Context, src audioSource, sink *ring, status func(string)) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		// S_FALSE (already initialised) is fine
		if oleErr, ok := err.(*ole.OleError); !ok || oleErr.Code() != 1 {
			status(fmt.Sprintf("%s: COM init failed: %v", src, err))
			return
		}
	}
	defer ole.CoUninitialize()

	backoff := time.Second
	lastErr := ""
	for ctx.Err() == nil {
		err := captureOnce(ctx, &src, sink, status)
		if ctx.Err() != nil {
			return
		}
		if err != nil && err.Error() != lastErr { // say it once, keep retrying quietly
			lastErr = err.Error()
			switch {
			case src.kind == srcMic && strings.HasPrefix(lastErr, "no default"):
				status("no microphone found: recording PC audio only (plug one in and it will be picked up)")
			case src.app.Exe != "" && strings.HasPrefix(lastErr, "app closed"):
				status(fmt.Sprintf("%s closed: waiting for it to come back", src.app.Name))
			default:
				status(fmt.Sprintf("%s: %v (retrying)", src, err))
			}
		} else if err == nil {
			lastErr = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
}

// openSource gets an audio client for src, with the format to request and the stream
// flags, and a function that says when to reopen (default device changed, app gone).
func openSource(enum *wca.IMMDeviceEnumerator, src *audioSource, status func(string)) (
	client *wca.IAudioClient, wfx *wca.WAVEFORMATEX, flags uint32, reopen func() bool, cleanup func(), err error) {
	cleanup = func() {}
	if src.kind == srcSystem && src.app.Exe != "" {
		// one app: find it again if it was restarted
		if !appRunning(src.app.PID) {
			app, ok := resolveApp(src.app.Exe)
			if !ok {
				return nil, nil, 0, nil, cleanup, fmt.Errorf("app closed")
			}
			src.app = app
		}
		client, err = activateAppLoopback(src.app.PID)
		if err != nil {
			return nil, nil, 0, nil, cleanup, fmt.Errorf("capturing %s: %w", src.app.Name, err)
		}
		// app capture has no mix format of its own: ask for 48 kHz stereo float,
		// Windows converts whatever the app plays
		f := &wca.WAVEFORMATEX{WFormatTag: 3, NChannels: 2, NSamplesPerSec: 48000, NAvgBytesPerSec: 48000 * 8,
			NBlockAlign: 8, WBitsPerSample: 32}
		const autoConvert, srcDefaultQuality, eventCallback = 0x80000000, 0x08000000, 0x00040000
		pid := src.app.PID
		return client, f, wca.AUDCLNT_STREAMFLAGS_LOOPBACK | eventCallback | autoConvert | srcDefaultQuality,
			func() bool { return !appRunning(pid) }, func() {}, nil
	}

	flow := uint32(wca.ECapture)
	if src.kind == srcSystem {
		flow = wca.ERender
	}
	var dev *wca.IMMDevice
	followDefault := true
	if src.kind == srcMic && src.deviceID != "" {
		if dev, err = getDevice(enum, src.deviceID); err == nil {
			var state uint32
			if dev.GetState(&state) != nil || state != wca.DEVICE_STATE_ACTIVE {
				dev.Release()
				dev, err = nil, fmt.Errorf("unplugged")
			}
		}
		if err != nil {
			status("the chosen microphone isn't available: using the Windows default")
			src.deviceID, err = "", nil
		} else {
			followDefault = false
		}
	}
	if dev == nil {
		if err := enum.GetDefaultAudioEndpoint(flow, wca.EConsole, &dev); err != nil {
			return nil, nil, 0, nil, cleanup, fmt.Errorf("no default %s device", map[bool]string{true: "mic", false: "output"}[src.kind == srcMic])
		}
	}
	var devID string
	_ = dev.GetId(&devID)
	client, err = activateAudioClient(dev)
	dev.Release()
	if err != nil {
		return nil, nil, 0, nil, cleanup, fmt.Errorf("activate: %w", err)
	}
	if err = client.GetMixFormat(&wfx); err != nil {
		client.Release()
		return nil, nil, 0, nil, cleanup, fmt.Errorf("mix format: %w", err)
	}
	cleanup = func() { ole.CoTaskMemFree(uintptr(unsafe.Pointer(wfx))) }
	if src.kind == srcSystem {
		flags = wca.AUDCLNT_STREAMFLAGS_LOOPBACK
	}
	reopen = func() bool {
		if !followDefault {
			return false
		}
		var d *wca.IMMDevice
		if enum.GetDefaultAudioEndpoint(flow, wca.EConsole, &d) != nil {
			return false
		}
		defer d.Release()
		var id string
		_ = d.GetId(&id)
		return id != "" && id != devID
	}
	return client, wfx, flags, reopen, cleanup, nil
}

// captureOnce runs until the device fails, the default device changes (or the app
// closes), or ctx ends.
func captureOnce(ctx context.Context, src *audioSource, sink *ring, status func(string)) error {
	var enum *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &enum); err != nil {
		return fmt.Errorf("device enumerator: %w", err)
	}
	defer enum.Release()

	client, wfx, flags, reopen, cleanup, err := openSource(enum, src, status)
	if err != nil {
		return err
	}
	defer client.Release()
	defer cleanup()
	format, err := describeFormat(wfx)
	if err != nil {
		return err
	}
	const bufferDuration = 2_000_000 // 200 ms in 100 ns units
	if err := client.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, bufferDuration, 0, wfx, nil); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if flags&0x00040000 != 0 { // event-driven streams need an event even though we poll
		ev, _ := windows.CreateEvent(nil, 0, 0, nil)
		defer windows.CloseHandle(ev)
		if err := client.SetEventHandle(uintptr(ev)); err != nil {
			return fmt.Errorf("event: %w", err)
		}
	}
	var cap *wca.IAudioCaptureClient
	if err := client.GetService(wca.IID_IAudioCaptureClient, &cap); err != nil {
		return fmt.Errorf("capture client: %w", err)
	}
	defer cap.Release()
	if err := client.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer client.Stop()

	status(fmt.Sprintf("%s: %d Hz, %d ch", src, format.rate, format.channels))
	rs := newResampler(format.rate, TargetRate)
	mono := make([]float32, 0, 4800)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	lastCheck := time.Now()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		for {
			var frames uint32
			if err := cap.GetNextPacketSize(&frames); err != nil {
				return fmt.Errorf("device lost: %w", err)
			}
			if frames == 0 {
				break
			}
			var data *byte
			var bufFlags uint32
			if err := cap.GetBuffer(&data, &frames, &bufFlags, nil, nil); err != nil {
				return fmt.Errorf("read: %w", err)
			}
			mono = mono[:0]
			if bufFlags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 || data == nil {
				for i := uint32(0); i < frames; i++ {
					mono = append(mono, 0)
				}
			} else {
				raw := unsafe.Slice(data, int(frames)*format.blockAlign)
				mono = format.toMono(raw, int(frames), mono)
			}
			if err := cap.ReleaseBuffer(frames); err != nil {
				return fmt.Errorf("release: %w", err)
			}
			sink.write(rs.process(mono))
		}
		if time.Since(lastCheck) > 2*time.Second {
			lastCheck = time.Now()
			if reopen() {
				if src.app.Exe != "" {
					return fmt.Errorf("app closed")
				}
				status(fmt.Sprintf("%s: default device changed, switching", src))
				return nil
			}
		}
	}
}

// activateAudioClient calls IMMDevice::Activate directly: go-wca v0.3.0 passes the
// CLSCTX argument by pointer instead of by value, which Windows rejects with
// E_INVALIDARG ("The parameter is incorrect").
func activateAudioClient(dev *wca.IMMDevice) (*wca.IAudioClient, error) {
	var client *wca.IAudioClient
	hr, _, _ := syscall.SyscallN(dev.VTable().Activate,
		uintptr(unsafe.Pointer(dev)),
		uintptr(unsafe.Pointer(wca.IID_IAudioClient)),
		uintptr(wca.CLSCTX_ALL),
		0,
		uintptr(unsafe.Pointer(&client)))
	if hr != 0 {
		return nil, ole.NewError(hr)
	}
	return client, nil
}

type pcmFormat struct {
	rate, channels, blockAlign, bits int
	float                            bool
}

func describeFormat(w *wca.WAVEFORMATEX) (pcmFormat, error) {
	f := pcmFormat{rate: int(w.NSamplesPerSec), channels: int(w.NChannels), blockAlign: int(w.NBlockAlign), bits: int(w.WBitsPerSample)}
	switch w.WFormatTag {
	case 3: // WAVE_FORMAT_IEEE_FLOAT
		f.float = true
	case 1: // WAVE_FORMAT_PCM
	case 0xFFFE: // WAVE_FORMAT_EXTENSIBLE: sub-format GUID's first 4 bytes are at offset 24
		sub := *(*uint32)(unsafe.Add(unsafe.Pointer(w), 24))
		f.float = sub == 3
	default:
		return f, fmt.Errorf("unsupported audio format tag 0x%x", w.WFormatTag)
	}
	if f.float && f.bits != 32 || !f.float && f.bits != 16 && f.bits != 24 && f.bits != 32 {
		return f, fmt.Errorf("unsupported sample size %d bits", f.bits)
	}
	return f, nil
}

// toMono averages the channels of one buffer into out.
func (f pcmFormat) toMono(raw []byte, frames int, out []float32) []float32 {
	bytesPer := f.bits / 8
	for i := 0; i < frames; i++ {
		var sum float32
		base := i * f.blockAlign
		for c := 0; c < f.channels; c++ {
			p := raw[base+c*bytesPer:]
			switch {
			case f.float:
				sum += *(*float32)(unsafe.Pointer(&p[0]))
			case f.bits == 16:
				sum += float32(int16(uint16(p[0])|uint16(p[1])<<8)) / 32768
			case f.bits == 24:
				v := int32(uint32(p[0])<<8|uint32(p[1])<<16|uint32(p[2])<<24) >> 8
				sum += float32(v) / 8388608
			default: // 32-bit int
				v := int32(uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16 | uint32(p[3])<<24)
				sum += float32(v) / 2147483648
			}
		}
		out = append(out, sum/float32(f.channels))
	}
	return out
}
