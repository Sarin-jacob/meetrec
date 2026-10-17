//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// Installs into temporary folders and a throwaway registry key (not the real PATH).
func TestInstall(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "local"))
	t.Setenv("APPDATA", filepath.Join(tmp, "roaming"))
	oldKey, oldVersion := envKey, version
	envKey = `Software\meetrec-test\Environment`
	defer func() {
		envKey, version = oldKey, oldVersion
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\meetrec-test\Environment`)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\meetrec-test`)
	}()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, envKey, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	_ = k.SetExpandStringValue("Path", `C:\Some\Tool;%USERPROFILE%\bin`)
	k.Close()
	pathValue := func() string {
		k, _ := registry.OpenKey(registry.CURRENT_USER, envKey, registry.QUERY_VALUE)
		defer k.Close()
		v, _, _ := k.GetStringValue("Path")
		return v
	}

	version = "0.2.0"
	msg, err := install()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "Installed meetrec 0.2.0") {
		t.Errorf("first install message: %q", msg)
	}
	if _, err := os.Stat(installedExe()); err != nil {
		t.Errorf("exe not copied: %v", err)
	}
	if got := installedVersion(); got != "0.2.0" {
		t.Errorf("installed version %q", got)
	}
	if p := pathValue(); !strings.HasPrefix(p, `C:\Some\Tool;%USERPROFILE%\bin;`) || !strings.HasSuffix(p, installDir()) {
		t.Errorf("PATH not appended correctly: %q", p)
	}
	if _, err := os.Stat(shortcutPath()); err != nil {
		t.Errorf("no Start menu shortcut: %v", err)
	}

	// same version again: nothing to do, PATH not duplicated
	if msg, err := install(); err != nil || msg != "" {
		t.Errorf("second run: %q %v", msg, err)
	}
	if n := strings.Count(strings.ToLower(pathValue()), strings.ToLower(installDir())); n != 1 {
		t.Errorf("install dir appears %d times in PATH", n)
	}
	// an older download doesn't downgrade
	version = "0.1.9"
	if msg, _ := install(); msg != "" {
		t.Errorf("downgraded: %q", msg)
	}
	// a newer one updates
	version = "0.10.0"
	if msg, err := install(); err != nil || !strings.Contains(msg, "from 0.2.0 to 0.10.0") {
		t.Errorf("update: %q %v", msg, err)
	}

	if err := removeFromPath(installDir()); err != nil {
		t.Fatal(err)
	}
	if p := pathValue(); p != `C:\Some\Tool;%USERPROFILE%\bin` {
		t.Errorf("PATH after removal: %q", p)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"0.10.0", "0.9.1", true}, {"0.1.0", "0.1.0", false}, {"0.1.0", "0", true}, {"v1.0.0", "0.9", true}, {"0.1", "0.1.1", false}} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
