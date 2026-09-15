//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

func hideConsoleWindow() {}

func showConsoleWindow() {}

func openBrowserURL(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "linux":
		cmd = "xdg-open"
		args = []string{url}
	default:
		fmt.Printf("Open this URL in your browser: %s\n", url)
		return
	}
	if err := exec.Command(cmd, args...).Start(); err != nil {
		fmt.Printf("Failed to open browser: %v\nURL: %s\n", err, url)
	}
}

// triggerShutdownFlush mirrors the Windows-only var (window_windows.go): set in
// main() to flush pending debounced index saves before a hard kill. Non-Windows
// has no console-close interception, so it stays nil here; declaring it on all
// platforms keeps main.go buildable for Linux/containers.
var triggerShutdownFlush func()

func preventConsoleClose(triggerQuit func()) bool {
	return false
}

// showFatalError surfaces a fatal error before exit. No-op on non-Windows
// (console apps always have a visible stderr there).
func showFatalError(text string) {
	fmt.Fprintln(os.Stderr, text)
}

// showFirstRunNotice is a no-op on non-Windows (no tray-only mode there).
func showFirstRunNotice() {}

// configureChildWindow is a no-op on non-Windows (the console-window-hiding
// concern is Windows-specific). Defined for build-portability.
func configureChildWindow(cmd *exec.Cmd) {}