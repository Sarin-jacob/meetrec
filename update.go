package main

// Updates: the latest GitHub release of meetrec, whose meetrec.exe is checked against the
// meetrec.exe.sha256 published with it (see .github/workflows/release.yml), then put in
// place of the installed copy the same way a newer download installs itself.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// where to ask; tests point it at a fake server, and MEETREC_RELEASES_URL at a test release
var releasesURL = "https://api.github.com/repos/Sarin-jacob/meetrec/releases/latest"

func init() {
	if u := os.Getenv("MEETREC_RELEASES_URL"); u != "" {
		releasesURL = u
	}
}

const maxExeSize = 100 << 20

type release struct {
	Version string // "0.5.0"
	ExeURL  string
	SumURL  string
}

var updateHTTP = &http.Client{Timeout: 5 * time.Minute}

func get(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "meetrec/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("%s said %d", strings.SplitN(strings.TrimPrefix(url, "https://"), "/", 2)[0], resp.StatusCode)
	}
	return resp, nil
}

// latestRelease: the newest published meetrec, with its exe and checksum.
func latestRelease() (release, error) {
	resp, err := get(releasesURL)
	if err != nil {
		return release{}, fmt.Errorf("checking for updates: %w", err)
	}
	defer resp.Body.Close()
	var r struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return release{}, fmt.Errorf("checking for updates: %w", err)
	}
	rel := release{Version: strings.TrimPrefix(r.Tag, "v")}
	for _, a := range r.Assets {
		switch a.Name {
		case "meetrec.exe":
			rel.ExeURL = a.URL
		case "meetrec.exe.sha256":
			rel.SumURL = a.URL
		}
	}
	if rel.Version == "" || rel.ExeURL == "" || rel.SumURL == "" {
		return release{}, errors.New("checking for updates: the latest release has no meetrec.exe and checksum")
	}
	return rel, nil
}

// download saves the release's exe as dir/meetrec.update.exe, only if it matches the
// published checksum and looks like a Windows program.
func (r release) download(dir string) (string, error) {
	resp, err := get(r.SumURL)
	if err != nil {
		return "", err
	}
	sumText, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	want := strings.ToLower(strings.Fields(string(sumText) + " ")[0])
	if len(want) != 64 {
		return "", errors.New("the release's checksum file is unreadable")
	}

	resp, err = get(r.ExeURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	path := filepath.Join(dir, "meetrec.update.exe")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxExeSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	head := make([]byte, 2)
	if err == nil {
		if rf, oerr := os.Open(path); oerr == nil {
			_, _ = io.ReadFull(rf, head)
			rf.Close()
		}
	}
	switch {
	case err != nil:
	case n > maxExeSize:
		err = errors.New("the download is too big")
	case hex.EncodeToString(h.Sum(nil)) != want:
		err = errors.New("the download doesn't match the release's checksum")
	case string(head) != "MZ":
		err = errors.New("the download isn't a Windows program")
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// replaceExe puts newExe (in dir) in place of dir/meetrec.exe, even while that one runs: a
// running exe can't be overwritten, but it can be renamed out of the way.
func replaceExe(dir, newExe, newVersion string) error {
	target := filepath.Join(dir, "meetrec.exe")
	if _, err := os.Stat(target); err == nil {
		old := filepath.Join(dir, "meetrec.old.exe")
		_ = os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			return fmt.Errorf("couldn't replace %s: %w", target, err)
		}
	}
	if err := os.Rename(newExe, target); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "version.txt"), []byte(newVersion), 0o644)
}

// updateInstalled brings the installed copy (in dir, at version have) up to the latest
// release. It returns that release and whether anything was installed.
func updateInstalled(dir, have string) (release, bool, error) {
	rel, err := latestRelease()
	if err != nil {
		return rel, false, err
	}
	if !newer(rel.Version, have) {
		return rel, false, nil
	}
	if err := rel.install(dir); err != nil {
		return rel, false, err
	}
	return rel, true, nil
}

// install downloads the release and puts it in place in dir.
func (r release) install(dir string) error {
	exe, err := r.download(dir)
	if err != nil {
		return err
	}
	if err := replaceExe(dir, exe, r.Version); err != nil {
		_ = os.Remove(exe)
		return err
	}
	return nil
}
