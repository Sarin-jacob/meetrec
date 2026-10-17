//go:build windows

package main

// Where the sound comes from:
//   - microphones: the Windows default or a chosen device;
//   - PC sound: everything the PC plays (loopback of the default output), or just one
//     app and its child processes (Windows "process loopback", build 20348+, i.e.
//     Windows 11): a meeting app without notification sounds, music or videos.

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
)

type micDevice struct{ ID, Name string }

type audioApp struct {
	PID  uint32
	Exe  string // lower-case file name, e.g. "zoom.exe": what we remember
	Name string // for people, e.g. "Zoom"
	Path string // full path, for the icon
}

// Friendly names for apps people meet in.
var appNames = map[string]string{
	"zoom.exe": "Zoom", "ms-teams.exe": "Teams", "teams.exe": "Teams", "chrome.exe": "Chrome",
	"msedge.exe": "Edge", "firefox.exe": "Firefox", "brave.exe": "Brave", "opera.exe": "Opera",
	"slack.exe": "Slack", "discord.exe": "Discord", "webex.exe": "Webex", "ciscocollabhost.exe": "Webex",
	"atmgr.exe": "Webex", "skype.exe": "Skype", "whatsapp.exe": "WhatsApp", "telegram.exe": "Telegram",
	"signal.exe": "Signal", "gotomeeting.exe": "GoTo", "g2mcomm.exe": "GoTo", "bluejeans.exe": "BlueJeans",
	"jitsi meet.exe": "Jitsi", "lark.exe": "Lark", "feishu.exe": "Feishu", "dingtalk.exe": "DingTalk",
}

func appDisplayName(exe string) string {
	exe = strings.ToLower(exe)
	if n, ok := appNames[exe]; ok {
		return n
	}
	base := strings.TrimSuffix(exe, filepath.Ext(exe))
	if base == "" {
		return exe
	}
	return strings.ToUpper(base[:1]) + base[1:]
}

// Processes that start apps; an app's "root" is the top process below these.
var launchers = map[string]bool{
	"explorer.exe": true, "svchost.exe": true, "services.exe": true, "wininit.exe": true, "sihost.exe": true,
	"runtimebroker.exe": true, "cmd.exe": true, "powershell.exe": true, "pwsh.exe": true,
	"windowsterminal.exe": true, "openconsole.exe": true, "conhost.exe": true, "userinit.exe": true,
	"applicationframehost.exe": true, "smss.exe": true, "csrss.exe": true, "winlogon.exe": true,
}

type procEntry struct {
	parent uint32
	exe    string // lower case
}

func processTable() map[uint32]procEntry {
	out := map[uint32]procEntry{}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return out
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		out[e.ProcessID] = procEntry{parent: e.ParentProcessID, exe: strings.ToLower(windows.UTF16ToString(e.ExeFile[:]))}
	}
	return out
}

// appRoot: the top process of the app that pid belongs to (Chrome's audio helper ->
// the main chrome.exe; Teams' webview -> ms-teams.exe), so the whole app is captured.
func appRoot(pid uint32, procs map[uint32]procEntry) uint32 {
	seen := map[uint32]bool{}
	for {
		p, ok := procs[pid]
		if !ok || seen[pid] {
			return pid
		}
		seen[pid] = true
		parent, ok := procs[p.parent]
		if !ok || launchers[parent.exe] || p.parent == 0 || p.parent == pid {
			return pid
		}
		pid = p.parent
	}
}

func processPath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// resolveApp finds the running app with this exe name. There can be several copies
// (two Chrome profiles, a desktop app and a helper with the same name): prefer the
// one that has a sound stream open, playing first.
func resolveApp(exe string) (audioApp, bool) {
	if apps, err := listAudioApps(); err == nil {
		for _, a := range apps {
			if strings.EqualFold(a.Exe, exe) {
				return a, true
			}
		}
	}
	return findApp(exe)
}

// findApp: the running root process of the app with this exe name.
func findApp(exe string) (audioApp, bool) {
	exe = strings.ToLower(exe)
	procs := processTable()
	var pids []uint32
	for pid, p := range procs {
		if p.exe == exe && procs[p.parent].exe != exe { // a root, not one of its helpers
			pids = append(pids, pid)
		}
	}
	if len(pids) == 0 {
		return audioApp{}, false
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	pid := pids[0] // its child processes (helpers, webviews) are captured with it
	return audioApp{PID: pid, Exe: procs[pid].exe, Name: appDisplayName(procs[pid].exe), Path: processPath(pid)}, true
}

func appRunning(pid uint32) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, _ := windows.WaitForSingleObject(h, 0)
	return ev == uint32(windows.WAIT_TIMEOUT)
}

// appCaptureSupported: per-app capture needs Windows build 20348 or later.
func appCaptureSupported() bool {
	return windows.RtlGetVersion().BuildNumber >= 20348
}

// withCOM runs f on a locked thread with COM initialised (multithreaded).
func withCOM(f func() error) error {
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if e := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); e != nil {
			if oe, ok := e.(*ole.OleError); !ok || oe.Code() != 1 {
				err = e
				return
			}
		}
		defer ole.CoUninitialize()
		err = f()
	}()
	<-done
	return err
}

// listMics: active recording devices; the Windows default first.
func listMics() ([]micDevice, string, error) {
	var mics []micDevice
	var defName string
	err := withCOM(func() error {
		var enum *wca.IMMDeviceEnumerator
		if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &enum); err != nil {
			return err
		}
		defer enum.Release()
		defID := ""
		var def *wca.IMMDevice
		if enum.GetDefaultAudioEndpoint(wca.ECapture, wca.EConsole, &def) == nil {
			_ = def.GetId(&defID)
			defName = deviceName(def)
			def.Release()
		}
		var coll *wca.IMMDeviceCollection
		if err := enum.EnumAudioEndpoints(wca.ECapture, wca.DEVICE_STATE_ACTIVE, &coll); err != nil {
			return err
		}
		defer coll.Release()
		var n uint32
		_ = coll.GetCount(&n)
		for i := uint32(0); i < n; i++ {
			var dev *wca.IMMDevice
			if coll.Item(i, &dev) != nil {
				continue
			}
			var id string
			_ = dev.GetId(&id)
			mics = append(mics, micDevice{ID: id, Name: deviceName(dev)})
			dev.Release()
		}
		sort.SliceStable(mics, func(i, j int) bool { return mics[i].ID == defID && mics[j].ID != defID })
		return nil
	})
	return mics, defName, err
}

func deviceName(dev *wca.IMMDevice) string {
	var ps *wca.IPropertyStore
	if err := dev.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return "Microphone"
	}
	defer ps.Release()
	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return "Microphone"
	}
	return pv.String()
}

// listAudioApps: apps that have opened a sound stream on the default output device
// (playing now or recently), as their top process, playing ones first.
func listAudioApps() ([]audioApp, error) {
	type found struct {
		app    audioApp
		active bool
	}
	byRoot := map[uint32]*found{}
	err := withCOM(func() error {
		var enum *wca.IMMDeviceEnumerator
		if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &enum); err != nil {
			return err
		}
		defer enum.Release()
		var dev *wca.IMMDevice
		if err := enum.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &dev); err != nil {
			return err
		}
		defer dev.Release()
		var mgr *wca.IAudioSessionManager2
		if err := activateInterface(dev, wca.IID_IAudioSessionManager2, unsafe.Pointer(&mgr)); err != nil {
			return err
		}
		defer mgr.Release()
		var sessions *wca.IAudioSessionEnumerator
		if err := mgr.GetSessionEnumerator(&sessions); err != nil {
			return err
		}
		defer sessions.Release()
		var n int
		_ = sessions.GetCount(&n)
		procs := processTable()
		for i := 0; i < n; i++ {
			var ctl *wca.IAudioSessionControl
			if sessions.GetSession(i, &ctl) != nil {
				continue
			}
			disp, err := ctl.QueryInterface(wca.IID_IAudioSessionControl2)
			var state uint32
			_ = ctl.GetState(&state)
			ctl.Release()
			if err != nil {
				continue
			}
			ctl2 := (*wca.IAudioSessionControl2)(unsafe.Pointer(disp))
			var pid uint32
			_ = ctl2.GetProcessId(&pid)
			ctl2.Release()
			if pid == 0 || state == wca.AudioSessionStateExpired {
				continue // system sounds, or a stream that's gone
			}
			if _, alive := procs[pid]; !alive {
				continue
			}
			root := appRoot(pid, procs)
			f := byRoot[root]
			if f == nil {
				f = &found{app: audioApp{PID: root, Exe: procs[root].exe, Name: appDisplayName(procs[root].exe)}}
				byRoot[root] = f
			}
			f.active = f.active || state == wca.AudioSessionStateActive
		}
		return nil
	})
	var out []audioApp
	var list []*found
	for _, f := range byRoot {
		list = append(list, f)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].active != list[j].active {
			return list[i].active
		}
		return list[i].app.Name < list[j].app.Name
	})
	for _, f := range list {
		f.app.Path = processPath(f.app.PID)
		out = append(out, f.app)
	}
	return out, err
}

// activateInterface: IMMDevice::Activate for any interface (see activateAudioClient).
func activateInterface(dev *wca.IMMDevice, iid *ole.GUID, out unsafe.Pointer) error {
	hr, _, _ := syscall.SyscallN(dev.VTable().Activate, uintptr(unsafe.Pointer(dev)), uintptr(unsafe.Pointer(iid)),
		uintptr(wca.CLSCTX_ALL), 0, uintptr(out))
	if hr != 0 {
		return ole.NewError(hr)
	}
	return nil
}

// getDevice: IMMDeviceEnumerator::GetDevice (go-wca leaves it unimplemented).
func getDevice(enum *wca.IMMDeviceEnumerator, id string) (*wca.IMMDevice, error) {
	var dev *wca.IMMDevice
	hr, _, _ := syscall.SyscallN(enum.VTable().GetDevice, uintptr(unsafe.Pointer(enum)),
		uintptr(unsafe.Pointer(u16(id))), uintptr(unsafe.Pointer(&dev)))
	if hr != 0 {
		return nil, ole.NewError(hr)
	}
	return dev, nil
}

// ---------------------------------------------------------------- per-app capture

var (
	mmdevapi                     = windows.NewLazySystemDLL("mmdevapi.dll")
	pActivateAudioInterfaceAsync = mmdevapi.NewProc("ActivateAudioInterfaceAsync")
	iidCompletionHandler         = ole.NewGUID("{41D949AB-9862-444A-80F6-C261334DA5EB}")
	iidAgileObject               = ole.NewGUID("{94EA2B94-E9CC-49E0-C0FF-EE64CA8F5B90}")
	handlerVtblOnce              sync.Once
	handlerVtbl                  *completionHandlerVtbl
	handlersMu                   sync.Mutex
	liveHandlers                 = map[uintptr]*completionHandler{} // keep them reachable while Windows holds them
)

type completionHandlerVtbl struct {
	QueryInterface, AddRef, Release, ActivateCompleted uintptr
}

// completionHandler is a minimal COM object implementing
// IActivateAudioInterfaceCompletionHandler (and IAgileObject, which Windows requires).
type completionHandler struct {
	vtbl *completionHandlerVtbl
	refs int32
	done windows.Handle
}

func newCompletionHandler() *completionHandler {
	handlerVtblOnce.Do(func() {
		handlerVtbl = &completionHandlerVtbl{
			QueryInterface: syscall.NewCallback(func(this *completionHandler, iid *ole.GUID, out *uintptr) uintptr {
				if ole.IsEqualGUID(iid, ole.IID_IUnknown) || ole.IsEqualGUID(iid, iidCompletionHandler) || ole.IsEqualGUID(iid, iidAgileObject) {
					*out = uintptr(unsafe.Pointer(this))
					atomic.AddInt32(&this.refs, 1)
					return 0
				}
				*out = 0
				return 0x80004002 // E_NOINTERFACE
			}),
			AddRef: syscall.NewCallback(func(this *completionHandler) uintptr {
				return uintptr(atomic.AddInt32(&this.refs, 1))
			}),
			Release: syscall.NewCallback(func(this *completionHandler) uintptr {
				return uintptr(atomic.AddInt32(&this.refs, -1))
			}),
			ActivateCompleted: syscall.NewCallback(func(this *completionHandler, op uintptr) uintptr {
				_ = windows.SetEvent(this.done)
				return 0
			}),
		}
	})
	ev, _ := windows.CreateEvent(nil, 1, 0, nil)
	h := &completionHandler{vtbl: handlerVtbl, refs: 1, done: ev}
	handlersMu.Lock()
	liveHandlers[uintptr(unsafe.Pointer(h))] = h
	handlersMu.Unlock()
	return h
}

func (h *completionHandler) free() {
	handlersMu.Lock()
	delete(liveHandlers, uintptr(unsafe.Pointer(h)))
	handlersMu.Unlock()
	windows.CloseHandle(h.done)
}

// activateAppLoopback returns an IAudioClient that captures what process pid (and its
// child processes) plays.
func activateAppLoopback(pid uint32) (*wca.IAudioClient, error) {
	if err := pActivateAudioInterfaceAsync.Find(); err != nil {
		return nil, fmt.Errorf("this Windows can't capture a single app")
	}
	params := struct {
		ActivationType uint32 // AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK
		TargetPID      uint32
		Mode           uint32 // PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE
	}{1, pid, 0}
	var pv struct { // PROPVARIANT holding a VT_BLOB
		vt       uint16
		r1, r2   uint16
		r3       uint16
		size     uint32
		_        uint32
		blobData uintptr
	}
	pv.vt = 65 // VT_BLOB
	pv.size = uint32(unsafe.Sizeof(params))
	pv.blobData = uintptr(unsafe.Pointer(&params))

	h := newCompletionHandler()
	defer h.free()
	var op *ole.IUnknown
	hr, _, _ := pActivateAudioInterfaceAsync.Call(uintptr(unsafe.Pointer(u16(`VAD\Process_Loopback`))),
		uintptr(unsafe.Pointer(wca.IID_IAudioClient)), uintptr(unsafe.Pointer(&pv)), uintptr(unsafe.Pointer(h)),
		uintptr(unsafe.Pointer(&op)))
	if hr != 0 {
		return nil, ole.NewError(hr)
	}
	defer op.Release()
	if ev, _ := windows.WaitForSingleObject(h.done, 10000); ev != windows.WAIT_OBJECT_0 {
		return nil, fmt.Errorf("Windows didn't answer the app capture request")
	}
	// IActivateAudioInterfaceAsyncOperation::GetActivateResult is right after IUnknown
	vtbl := *(*[4]uintptr)(unsafe.Pointer(op.RawVTable))
	var activateHR uintptr
	var unk *ole.IUnknown
	hr, _, _ = syscall.SyscallN(vtbl[3], uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(&activateHR)), uintptr(unsafe.Pointer(&unk)))
	if hr != 0 {
		return nil, ole.NewError(hr)
	}
	if int32(activateHR) < 0 {
		return nil, ole.NewError(activateHR)
	}
	defer unk.Release()
	disp, err := unk.QueryInterface(wca.IID_IAudioClient)
	if err != nil {
		return nil, err
	}
	return (*wca.IAudioClient)(unsafe.Pointer(disp)), nil
}
