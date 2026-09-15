package main

import (
	"fmt"
	"log"
	"sync/atomic"

	"fyne.io/systray"
)

func runTray(port int, serverReady, done <-chan struct{}, triggerQuit func()) {
	var exitedNormally atomic.Bool

	defer func() {
		if !exitedNormally.Load() {
			log.Println("[TRAY] Tray exited unexpectedly, initiating shutdown")
			showConsoleWindow()
			triggerQuit()
		}
	}()

	systray.Run(func() {
		systray.SetIcon(trayIconData)
		systray.SetTooltip(fmt.Sprintf("Media Viewer Server - Running on port %d", port))

		openBrowser := systray.AddMenuItem("Open in Browser", "Open the server in your default browser")
		// "Open in Browser" is disabled until the HTTP server is confirmed
		// listening, so clicking it never opens a URL the server can't yet
		// serve (which let a service-worker-cached shell render with no data).
		openBrowser.Disable()
		systray.AddSeparator()
		showWindow := systray.AddMenuItem("Show Window", "Show the console window")
		hideWindow := systray.AddMenuItem("Hide Window", "Hide the console window")
		systray.AddSeparator()
		quitItem := systray.AddMenuItem("Quit", "Shut down the server")

		// Enable "Open in Browser" once the server is ready. In a GUI-subsystem
		// (tray) build the server binds within moments of startup, but gating
		// here guarantees the browser only opens http://localhost:<port> when
		// the server can actually respond. Select on done too so this goroutine
		// exits cleanly if the tray is quit before the server becomes ready
		// (otherwise it would block forever on <-serverReady).
		go func() {
			select {
			case <-serverReady:
				openBrowser.Enable()
				log.Printf("[TRAY] Server ready — 'Open in Browser' enabled (http://localhost:%d)", port)
			case <-done:
			}
		}()

		go func() {
			for {
				select {
				case <-openBrowser.ClickedCh:
					url := fmt.Sprintf("http://localhost:%d", port)
					openBrowserURL(url)
				case <-showWindow.ClickedCh:
					showConsoleWindow()
				case <-hideWindow.ClickedCh:
					hideConsoleWindow()
				case <-quitItem.ClickedCh:
					exitedNormally.Store(true)
					triggerQuit()
					systray.Quit()
					return
				}
			}
		}()
	}, func() {
		log.Println("[TRAY] Tray exited")
	})
}