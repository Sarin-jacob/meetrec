//go:build windows

package main

// First run: copy meetrec.exe to %LOCALAPPDATA%\Programs\meetrec, put that folder on the
// user's PATH and add a Start menu entry, so a downloaded meetrec.exe can be run from
// anywhere afterwards. A newer download replaces the installed copy. No admin rights.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	pGetConsoleProcessList  = kernel32.NewProc("GetConsoleProcessList")
	pFreeConsole            = kernel32.NewProc("FreeConsole")
	pSendMessageTimeoutW    = user32.NewProc("SendMessageTimeoutW")
	envKey                  = `Environment` // HKCU key with the user's PATH (tests use another)
	errInstalledCopyIsNewer = errors.New("the installed meetrec is newer")
)

func installDir() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "meetrec")
}

func installedExe() string { return filepath.Join(installDir(), "meetrec.exe") }

func shortcutPath() string {
	return filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "meetrec.lnk")
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

func runningInstalled() bool {
	return strings.EqualFold(filepath.Clean(filepath.Dir(selfPath())), filepath.Clean(installDir()))
}

// consoleCount: processes sharing our console. 1 = Explorer opened a console just for us
// (double-click), 0 = no console (started detached), more = a terminal.
func consoleCount() int {
	var ids [4]uint32
	n, _, _ := pGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&ids[0])), 4)
	return int(n)
}

func ownConsole() bool { return consoleCount() == 1 }

// install copies this exe into place when it isn't the installed copy (or is newer than
// it). Returns a message for the user when it did something. Local "dev" builds only
// install when MEETREC_INSTALL is set.
func install() (string, error) {
	if version == "dev" && os.Getenv("MEETREC_INSTALL") == "" {
		return "", nil
	}
	self := selfPath()
	if self == "" || runningInstalled() {
		return "", nil
	}
	dir := installDir()
	have := installedVersion()
	if have != "" && !newer(version, have) {
		if err := addToPath(dir); err != nil { // keep PATH right even when nothing is copied
			return "", err
		}
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	target := installedExe()
	tmp := target + ".new"
	if err := copyFile(self, tmp); err != nil {
		return "", err
	}
	if _, err := os.Stat(target); err == nil {
		// a running exe can't be overwritten, but it can be renamed out of the way
		old := filepath.Join(dir, "meetrec.old.exe")
		_ = os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("couldn't replace %s: %w", target, err)
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", err
	}
	_ = os.WriteFile(filepath.Join(dir, "version.txt"), []byte(version), 0o644)
	if err := addToPath(dir); err != nil {
		return "", err
	}
	if err := makeShortcut(target); err != nil {
		return "", fmt.Errorf("Start menu entry: %w", err)
	}
	if have == "" {
		return fmt.Sprintf("Installed meetrec %s in %s and added it to your PATH: in a new terminal, just type `meetrec`. "+
			"The floating recorder is in the Start menu (meetrec).", version, dir), nil
	}
	return fmt.Sprintf("Updated the installed meetrec from %s to %s.", have, version), nil
}

func installedVersion() string {
	if _, err := os.Stat(installedExe()); err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(installDir(), "version.txt"))
	if err != nil {
		return "0"
	}
	return strings.TrimSpace(string(b))
}

// newer: is version a newer than b? ("0.10.0" > "0.9.1"; anything > "0")
func newer(a, b string) bool {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// cleanupOldExe removes the copy an update renamed aside (once it's no longer running).
func cleanupOldExe() {
	_ = os.Remove(filepath.Join(installDir(), "meetrec.old.exe"))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func pathEntries(path string) []string {
	var out []string
	for _, p := range strings.Split(path, ";") {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(os.ExpandEnv(strings.TrimSpace(a))), filepath.Clean(b))
}

// addToPath appends dir to the user's PATH (HKCU\Environment) if it isn't there yet and
// tells running programs, so new terminals see it.
func addToPath(dir string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, envKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	cur, _, err := k.GetStringValue("Path")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	entries := pathEntries(cur)
	for _, e := range entries {
		if samePath(e, dir) {
			return nil
		}
	}
	if err := k.SetExpandStringValue("Path", strings.Join(append(entries, dir), ";")); err != nil {
		return err
	}
	broadcastEnvChange()
	return nil
}

func removeFromPath(dir string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, envKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	cur, _, err := k.GetStringValue("Path")
	if err != nil {
		return nil
	}
	var keep []string
	for _, e := range pathEntries(cur) {
		if !samePath(e, dir) {
			keep = append(keep, e)
		}
	}
	if err := k.SetExpandStringValue("Path", strings.Join(keep, ";")); err != nil {
		return err
	}
	broadcastEnvChange()
	return nil
}

func broadcastEnvChange() {
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x1a, 2
	env, _ := syscall.UTF16PtrFromString("Environment")
	var res uintptr
	pSendMessageTimeoutW.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)), smtoAbortIfHung, 3000,
		uintptr(unsafe.Pointer(&res)))
}

// makeShortcut adds Start menu → meetrec, which opens the floating recorder. It starts
// minimised so the console window Windows creates for it doesn't flash up.
func makeShortcut(target string) error {
	runtime.LockOSThread() // COM objects belong to the thread that created them
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		if oe, ok := err.(*ole.OleError); !ok || oe.Code() != 1 { // S_FALSE: already initialised
			return err
		}
	}
	defer ole.CoUninitialize()
	unk, err := oleutil.CreateObject("WScript.Shell")
	if err != nil {
		return err
	}
	defer unk.Release()
	shell, err := unk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	defer shell.Release()
	if err := os.MkdirAll(filepath.Dir(shortcutPath()), 0o755); err != nil {
		return err
	}
	v, err := oleutil.CallMethod(shell, "CreateShortcut", shortcutPath())
	if err != nil {
		return fmt.Errorf("CreateShortcut: %w", err)
	}
	sc := v.ToIDispatch()
	defer sc.Release()
	for name, val := range map[string]any{
		"TargetPath": target, "Arguments": "gui", "WorkingDirectory": filepath.Dir(target),
		"Description": "Record online meetings for Meeting Notes", "WindowStyle": int32(7), "IconLocation": target + ",0",
	} {
		if _, err := oleutil.PutProperty(sc, name, val); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if _, err = oleutil.CallMethod(sc, "Save"); err != nil {
		if oe, ok := err.(*ole.OleError); ok {
			if ex, ok := oe.SubError().(ole.EXCEPINFO); ok {
				return fmt.Errorf("Save: %s", ex.Error())
			}
		}
		return fmt.Errorf("Save: %w", err)
	}
	return nil
}

// uninstall removes the PATH entry, the Start menu entry and the installed files.
// Recordings (~/MeetRec) and the sign-in (config.json) are kept.
func uninstall() error {
	dir := installDir()
	if err := removeFromPath(dir); err != nil {
		return err
	}
	_ = os.Remove(shortcutPath())
	if runningInstalled() {
		// can't delete our own exe while running: let cmd do it a moment after we exit
		cmd := exec.Command("cmd", "/c", "ping -n 3 127.0.0.1 >nul & rmdir /s /q \""+dir+"\"")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
		if err := cmd.Start(); err != nil {
			return err
		}
	} else if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Printf("Removed meetrec from %s, your PATH and the Start menu. Recordings and your sign-in are kept.\n", dir)
	return nil
}

// launchDetached starts meetrec again with args, with no console window, and returns.
func launchDetached(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return cmd.Start()
}
