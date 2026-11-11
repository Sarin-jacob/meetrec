//go:build windows

package main

// The floating recorder keeps itself up to date: it checks a minute after starting and then
// every 6 hours, installs a newer release in the background (when "Update automatically"
// is on, the default), and restarts into it at a quiet moment: not recording, not saving,
// not sending. Local "dev" builds and copies that aren't installed don't update.

import (
	"fmt"
	"log"
	"unsafe"
)

// how an update check was asked for
const (
	updBackground = iota // the timer, or "Update automatically" switched on
	updAsked             // "Check for updates" in the menu
	updInstall           // "Update to … now"
)

func updatesOn() bool { return version != "dev" && runningInstalled() }

// checkUpdates looks for a newer release off the UI thread, installs it when that's wanted,
// and reports back with wmUpdate.
func (g *gui) checkUpdates(how int) {
	if !updatesOn() {
		if how != updBackground {
			messageBox(g.overlay, fmt.Sprintf("This meetrec (%s) isn't the installed copy, so it doesn't update itself.", version), mbIconInfo)
		}
		return
	}
	g.upd.mu.Lock()
	if g.upd.checking {
		g.upd.mu.Unlock()
		return
	}
	g.upd.checking = true
	g.upd.mu.Unlock()
	install := how == updInstall || !g.cfg.GUI.NoAutoUpdate
	go func() {
		rel, err := latestRelease()
		ready := ""
		if err == nil && newer(rel.Version, version) && install {
			// already installed by an earlier check or `meetrec update`: only the restart is left
			if newer(rel.Version, installedVersion()) {
				err = rel.install(installDir())
			}
			if err == nil {
				ready = rel.Version
				log.Printf("meetrec %s installed, restarting into it when idle", rel.Version)
			}
		}
		g.upd.mu.Lock()
		g.upd.checking, g.upd.err = false, err
		if err == nil {
			g.upd.latest = rel.Version
		}
		if ready != "" {
			g.upd.ready = ready
		}
		g.upd.mu.Unlock()
		if err != nil {
			log.Printf("update: %v", err)
		}
		pPostMessageW.Call(g.overlay, wmUpdate, uintptr(how), 0)
	}()
}

// updateChecked: on the UI thread, after a check. Only checks someone asked for say anything.
func (g *gui) updateChecked(how int) {
	g.upd.mu.Lock()
	latest, ready, err := g.upd.latest, g.upd.ready, g.upd.err
	g.upd.mu.Unlock()
	switch {
	case how == updBackground:
	case err != nil:
		messageBox(g.overlay, "Couldn't update meetrec:\n"+err.Error()+"\n\nIt will try again later.", mbIconWarning)
	case ready != "":
		if !g.quiet() {
			messageBox(g.overlay, fmt.Sprintf("meetrec %s is installed. It starts as soon as you're done with this recording.", ready), mbIconInfo)
		}
	case newer(latest, version):
		if messageBox(g.overlay, fmt.Sprintf("meetrec %s is available (you have %s).\n\nUpdate now?", latest, version), mbYesNo|mbIconInfo) == idYes {
			g.checkUpdates(updInstall)
		}
	default:
		messageBox(g.overlay, fmt.Sprintf("You have the latest meetrec (%s).", version), mbIconInfo)
	}
	g.restartIfQuiet()
	g.redraw()
}

// quiet: nothing would be lost by restarting now.
func (g *gui) quiet() bool {
	return g.phase == phIdle && g.fin.hwnd == 0 && !g.dragging
}

// restartIfQuiet hands over to the newly installed version once nothing is going on.
func (g *gui) restartIfQuiet() {
	g.upd.mu.Lock()
	ready := g.upd.ready
	g.upd.mu.Unlock()
	if ready == "" || !g.quiet() {
		return
	}
	if err := relaunchGUI(version); err != nil {
		log.Printf("starting meetrec %s: %v", ready, err)
		g.upd.mu.Lock()
		g.upd.ready = "" // try again with the next check
		g.upd.mu.Unlock()
		return
	}
	log.Printf("restarting into meetrec %s", ready)
	g.quit()
}

// updateMenu: the menu's update items.
func (g *gui) updateMenu(menu uintptr, add func(m uintptr, flags uintptr, id int, text string), check func(bool) uintptr) {
	g.upd.mu.Lock()
	latest, ready, checking := g.upd.latest, g.upd.ready, g.upd.checking
	g.upd.mu.Unlock()
	switch {
	case ready != "":
		add(menu, mfGrayed, 0, fmt.Sprintf("meetrec %s starts when you're done", ready))
	case checking:
		add(menu, mfGrayed, 0, "Checking for updates…")
	case latest != "" && newer(latest, version):
		add(menu, mfString, cmdUpdateNow, fmt.Sprintf("Update to meetrec %s now", latest))
	default:
		add(menu, mfString, cmdCheckUpdate, fmt.Sprintf("Check for updates (%s)", version))
	}
	if updatesOn() {
		add(menu, mfString|check(!g.cfg.GUI.NoAutoUpdate), cmdAutoUpdate, "Update automatically")
	}
}

// pokeGUI tells a running floating recorder to check now (after `meetrec update`).
func pokeGUI() {
	if hw, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(u16("meetrecOverlay"+instance))), 0); hw != 0 {
		pPostMessageW.Call(hw, wmTimer, 3, 0)
	}
}
