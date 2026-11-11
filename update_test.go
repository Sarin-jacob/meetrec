package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeGitHub serves a "latest release" with this tag, exe and checksum file.
func fakeGitHub(t *testing.T, tag string, exe []byte, sum string) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":"meetrec.exe","browser_download_url":"%s/exe"},
			{"name":"meetrec.exe.sha256","browser_download_url":"%s/sum"}]}`, tag, srv.URL, srv.URL)
	})
	mux.HandleFunc("/exe", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(exe) })
	mux.HandleFunc("/sum", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, "%s  meetrec.exe\n", sum) })
	old := releasesURL
	releasesURL = srv.URL + "/latest"
	t.Cleanup(func() { releasesURL = old })
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func installedCopy(t *testing.T, have string) string {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "meetrec.exe"), []byte("MZ old"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "version.txt"), []byte(have), 0o644)
	return dir
}

func read(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdateInstallsANewerRelease(t *testing.T) {
	exe := []byte("MZ new program")
	fakeGitHub(t, "v0.5.0", exe, sha(exe))
	dir := installedCopy(t, "0.4.1")
	rel, done, err := updateInstalled(dir, "0.4.1")
	if err != nil || !done || rel.Version != "0.5.0" {
		t.Fatalf("got %+v %v %v", rel, done, err)
	}
	if read(t, filepath.Join(dir, "meetrec.exe")) != string(exe) || read(t, filepath.Join(dir, "version.txt")) != "0.5.0" {
		t.Error("new exe and version.txt in place")
	}
	if read(t, filepath.Join(dir, "meetrec.old.exe")) != "MZ old" {
		t.Error("the running copy is renamed out of the way, not deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "meetrec.update.exe")); err == nil {
		t.Error("no download left behind")
	}
}

func TestUpdateLeavesTheLatestAlone(t *testing.T) {
	exe := []byte("MZ same")
	fakeGitHub(t, "v0.4.1", exe, sha(exe))
	dir := installedCopy(t, "0.4.1")
	if _, done, err := updateInstalled(dir, "0.4.1"); err != nil || done {
		t.Fatalf("nothing to do, got done=%v err=%v", done, err)
	}
	if read(t, filepath.Join(dir, "meetrec.exe")) != "MZ old" {
		t.Error("installed copy untouched")
	}
}

func TestUpdateRefusesBadDownloads(t *testing.T) {
	for name, c := range map[string]struct {
		exe []byte
		sum string
	}{
		"checksum mismatch": {[]byte("MZ tampered"), sha([]byte("MZ original"))},
		"not a program":     {[]byte("<html>sign in</html>"), sha([]byte("<html>sign in</html>"))},
		"bad checksum file": {[]byte("MZ x"), "nope"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeGitHub(t, "v9.0.0", c.exe, c.sum)
			dir := installedCopy(t, "0.4.1")
			if _, _, err := updateInstalled(dir, "0.4.1"); err == nil {
				t.Fatal("should refuse")
			}
			if read(t, filepath.Join(dir, "meetrec.exe")) != "MZ old" || read(t, filepath.Join(dir, "version.txt")) != "0.4.1" {
				t.Error("installed copy untouched")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 2 {
				t.Errorf("nothing left behind: %v", entries)
			}
		})
	}
}

func TestLatestReleaseNeedsBothFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.0.0","assets":[{"name":"meetrec.exe","browser_download_url":"x"}]}`)
	}))
	defer srv.Close()
	old := releasesURL
	releasesURL = srv.URL
	defer func() { releasesURL = old }()
	if _, err := latestRelease(); err == nil {
		t.Error("a release without its checksum isn't installed")
	}
}
