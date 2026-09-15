//go:build windows

package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                 = windows.NewLazyDLL("kernel32.dll")
	user32                   = windows.NewLazyDLL("user32.dll")
	shell32                  = windows.NewLazyDLL("shell32.dll")
	procGetConsoleWindow     = kernel32.NewProc("GetConsoleWindow")
	procShowWindow           = user32.NewProc("ShowWindow")
	procSetForegroundWindow  = user32.NewProc("SetForegroundWindow")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	procShellExecuteW        = shell32.NewProc("ShellExecuteW")
	procGetWindowLongW       = user32.NewProc("GetWindowLongW")
	procSetWindowLongW       = user32.NewProc("SetWindowLongW")
	procSetWindowPos         = user32.NewProc("SetWindowPos")
	procAllocConsole         = kernel32.NewProc("AllocConsole")
	procMessageBoxW          = user32.NewProc("MessageBoxW")
	// WTSGetActiveConsoleSessionId is exported from kernel32.dll (despite the
	// "WTS" prefix). It is queried lazily via Find() (not Call()) because
	// LazyProc.Call() panics if the proc is missing on some Windows editions;
	// isInteractiveSession degrades to "interactive" when the proc is absent.
	procWTSGetActiveConsoleSessionId = kernel32.NewProc("WTSGetActiveConsoleSessionId")
)

const (
	SW_HIDE = 0
	SW_SHOW = 5

	// GWL_EXSTYLE = -20. Go has no negative uintptr literals;
	// ^uintptr(19) is the two's-complement representation of -20.
	GWL_EXSTYLE = ^uintptr(19)

	WS_EX_APPWINDOW  = 0x00040000
	WS_EX_TOOLWINDOW = 0x00000080

	// SetWindowPos flags
	SWP_NOMOVE       = 0x0002
	SWP_NOSIZE       = 0x0001
	SWP_NOZORDER     = 0x0004
	SWP_FRAMECHANGED = 0x0020

	CTRL_CLOSE_EVENT    = 2
	CTRL_LOGOFF_EVENT   = 5
	CTRL_SHUTDOWN_EVENT = 6

	// MessageBox flags
	MB_OK          = 0x00000000
	MB_ICONERROR   = 0x00000010
	MB_ICONINFORMATION = 0x00000040
	MB_TOPMOST     = 0x00040000
)

var consoleCtrlHandler uintptr
var triggerShutdown func()
// triggerShutdownFlush, when set, synchronously flushes any pending debounced
// index saves before the process can be hard-killed by the OS. It is wired up
// in main() to flushPendingIndexSaves(cfg, db). Called from the CTRL_CLOSE_EVENT
// handler because Windows terminates the process ~5s after the handler returns,
// so a background triggerShutdown goroutine may not finish in time.
var triggerShutdownFlush func()

// trayLogFile is the file that receives logs in tray mode (set by main.go).
// showConsoleWindow uses it to build an io.MultiWriter so that opening the
// on-demand console does NOT stop server.log from receiving new entries.
// Declared in main.go (shared across platforms); used here on Windows.

// hideConsoleWindow hides the console window (if any) so it leaves the screen
// and the taskbar. In a GUI-subsystem (tray) build there may be no console at
// all, in which case this is a no-op. If a console was allocated on demand by
// showConsoleWindow, hiding it (rather than freeing it) keeps the log stream
// attached so subsequent Show calls don't need to re-allocate.
func hideConsoleWindow() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		// No ownable console window (GUI-subsystem build with no console
		// allocated yet, or launched detached). Nothing to hide — and no
		// taskbar entry to remove.
		debugLog("[TRAY] hideConsoleWindow: no console window (GetConsoleWindow returned 0)")
		return
	}
	// Hide the window. A hidden window has no taskbar button, so SW_HIDE alone
	// reliably removes the taskbar entry. We deliberately do NOT toggle
	// WS_EX_TOOLWINDOW here: console windows are owned by conhost.exe and the
	// shell does not reliably re-evaluate taskbar membership on style changes,
	// which previously left a stale taskbar entry. Hiding is sufficient.
	procShowWindow.Call(hwnd, SW_HIDE)
	log.Println("[TRAY] Console window hidden")
}

// showStyledConsole applies the APPWINDOW extended style, forces the shell to
// re-evaluate taskbar presence, then shows and focuses the console window. It
// is shared by both showConsoleWindow branches (on-demand AllocConsole and
// already-existing console) so the two show paths cannot drift apart.
func showStyledConsole(hwnd uintptr) {
	exStyle, _, _ := procGetWindowLongW.Call(hwnd, GWL_EXSTYLE)
	newStyle := exStyle&^WS_EX_TOOLWINDOW | WS_EX_APPWINDOW
	procSetWindowLongW.Call(hwnd, GWL_EXSTYLE, newStyle)
	procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0,
		SWP_NOMOVE|SWP_NOSIZE|SWP_NOZORDER|SWP_FRAMECHANGED)
	procShowWindow.Call(hwnd, SW_SHOW)
	procSetForegroundWindow.Call(hwnd)
}

// showConsoleWindow shows the console window. In a GUI-subsystem (tray) build
// there is no console at startup, so this allocates one on demand via
// AllocConsole and reattaches the log output (which was redirected to a file
// in tray mode) to BOTH the new console's CONOUT$ and the file, so the user
// can see live logs while server.log keeps receiving entries. Repeated calls
// are idempotent: if a console already exists, it is just shown and focused.
func showConsoleWindow() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		// No console yet — allocate one. This happens in the GUI-subsystem
		// (tray) build when the user picks "Show Window".
		ret, _, err := procAllocConsole.Call()
		if ret == 0 {
			log.Printf("[TRAY] showConsoleWindow: AllocConsole failed: %v", err)
			return
		}
		// Reopen CONOUT$ so log output is visible in the newly allocated
		// console. Route log output to BOTH the console and the tray log file
		// via the existing thread-safe buffered writer's SetUnderlying, so the
		// buffer (and its coalescing) is preserved — previously SetOutput with a
		// MultiWriter bypassed the buffer and reintroduced per-line sync writes
		// to server.log for the rest of the session.
		if conout, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
			if trayLogWriter != nil {
				if trayLogFile != nil {
					_ = trayLogWriter.SetUnderlying(io.MultiWriter(trayLogFile, conout))
				} else {
					_ = trayLogWriter.SetUnderlying(conout)
				}
			} else if trayLogFile != nil {
				// No buffered writer (e.g. -notray fell through); fall back to
				// a plain MultiWriter so the file still receives entries.
				log.SetOutput(io.MultiWriter(trayLogFile, conout))
			} else {
				log.SetOutput(conout)
			}
		}
		// After AllocConsole, GetConsoleWindow now returns the new HWND.
		hwnd, _, _ = procGetConsoleWindow.Call()
		if hwnd == 0 {
			log.Println("[TRAY] showConsoleWindow: console allocated but no window handle")
			return
		}
		showStyledConsole(hwnd)
		log.Println("[TRAY] Console window allocated and shown (on-demand)")
		return
	}
	// Console already exists — show and focus it.
	showStyledConsole(hwnd)
	log.Println("[TRAY] Console window restored")
}

func openBrowserURL(url string) {
	procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("open"))),
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(url))),
		0, 0, SW_SHOW,
	)
}

// CREATE_NO_WINDOW prevents a console-subsystem child process from allocating a
// new (visible) console window when launched from this GUI-subsystem app. Used
// for the gotify-server.exe child so it doesn't create its own taskbar item.
const CREATE_NO_WINDOW = 0x08000000

// configureChildWindow hides any window a child process would otherwise create.
// On Windows, a console-subsystem child (e.g. gotify-server.exe) launched from a
// GUI-subsystem parent gets a fresh console window with its own taskbar entry,
// which doesn't hide with the parent's on-demand console. CREATE_NO_WINDOW makes
// the child run with no console window at all (its stdout/stderr still flow to
// the pipes wired by the caller). No-op on non-Windows.
func configureChildWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= CREATE_NO_WINDOW
	cmd.SysProcAttr.HideWindow = true
}

// messageBox shows a Win32 message box (works in a GUI-subsystem build with no
// console). flags is a combination of MB_* constants. Used to surface fatal
// errors (e.g. port-in-use) and first-run notices that would otherwise be
// invisible in tray mode. Returns true if the dialog was shown, false if it was
// skipped because the process is not attached to an interactive desktop (e.g.
// running in session 0 as a service), where MessageBoxW would otherwise block
// forever with no visible window.
func messageBox(text, caption string, flags uintptr) bool {
	if !isInteractiveSession() {
		return false
	}
	procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(text))),
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(caption))),
		flags,
	)
	return true
}

// isInteractiveSession reports whether this process is attached to an
// interactive desktop (not session 0 / a service context). MessageBoxW blocks
// forever when there is no interactive desktop, so callers must skip dialogs in
// that case. We treat session 0 (the services session) and a zero console
// session id as non-interactive.
//
// IMPORTANT: the proc is resolved with Find() (which returns an error) rather
// than Call() (which PANICS if the proc is missing). On Windows editions where
// WTSGetActiveConsoleSessionId is unavailable, we fail SAFE to non-interactive
// (suppress dialogs) — a skipped dialog is recoverable (the caller still exits
// and logs), but a MessageBoxW in a desktop-less session hangs forever.
func isInteractiveSession() bool {
	if err := procWTSGetActiveConsoleSessionId.Find(); err != nil {
		// Proc unavailable — fail SAFE: assume non-interactive and suppress the
		// dialog. "Proc missing" is orthogonal to "session interactive"; if the
		// proc is absent on a limited Windows edition that is also in a
		// service/session-0 desktop, calling MessageBoxW would block forever
		// with no desktop to render on. The fatal-error path still os.Exit()s
		// and logs to server.log, so a skipped dialog is recoverable; a hung
		// process is not. (On modern Windows the proc is always present, so
		// this branch is effectively unreachable in practice.)
		return false
	}
	// WTSGetActiveConsoleSessionId returns the session id of the physical
	// console. Session 0 is the services session (non-interactive for GUI).
	// A return of 0xFFFFFFFF means there is no console session at all.
	consoleSession, _, _ := procWTSGetActiveConsoleSessionId.Call()
	if consoleSession == 0xFFFFFFFF || consoleSession == 0 {
		// No physical console session OR the services session — not safe to
		// show a blocking dialog (it may have no desktop to render on).
		return false
	}
	return true
}

// showFatalError surfaces a fatal error to the user before the process exits.
// In a GUI-subsystem (tray) build there is no console, so a log.Fatalf would
// be invisible (written only to server.log). This pops a top-most error dialog
// so the user sees the failure (e.g. "port 3000 already in use") and can act.
func showFatalError(text string) {
	// If not in an interactive session, messageBox is a no-op; the caller must
	// still proceed to os.Exit (it does — showFatalError never blocks the exit).
	messageBox(text, "Media Viewer Server", MB_OK|MB_ICONERROR|MB_TOPMOST)
}

// showFirstRunNotice shows a one-time informational dialog explaining the
// tray-only behavior, the first time the app runs (gated by a marker file next
// to the executable). Subsequent runs are silent. Shown in a goroutine so it
// does not block startup.
func showFirstRunNotice() {
	markerPath := resolveRelativeToExe(".tray_notice_shown")
	if _, err := os.Stat(markerPath); err == nil {
		return // already shown
	}
	go func() {
		messageBox(
			"Media Viewer Server is running in the system tray.\n\n"+
				"There is no console window or taskbar item — look for the Media Viewer icon in the notification area.\n\n"+
				"Use the tray icon to: Open in Browser, Show Window (live logs), Hide Window, or Quit.",
			"Media Viewer Server",
			MB_OK|MB_ICONINFORMATION|MB_TOPMOST,
		)
		// Mark as shown so this never pops again (even if the dialog was
		// skipped in a non-interactive session, so it won't retry forever).
		_ = os.WriteFile(markerPath, []byte("1"), 0600)
	}()
}

func preventConsoleClose(triggerQuit func()) bool {
	triggerShutdown = triggerQuit

	handler := func(ctrlType uintptr) uintptr {
		switch ctrlType {
		case CTRL_CLOSE_EVENT:
			// This fires when the user closes an on-demand console (opened via
			// tray "Show Window") with its X button, or via Task Manager "End
			// Task" / GenerateConsoleCtrlEvent. We cannot cancel a console
			// close — Windows terminates the process after the handler returns
			// (within ~5s) — so do a graceful shutdown instead of pretending to
			// handle it.
			//
			// CRITICAL: synchronously flush any pending debounced index saves
			// BEFORE returning, because the background triggerShutdown goroutine
			// may not beat the OS's ~5s hard-kill deadline. This runs in the
			// console-control-handler thread under that deadline, so it must be
			// fast; saveSectionIndex uses atomic temp+rename so a mid-flush kill
			// leaves the prior index intact (corruption-safe). Surface the console
			// so the user can see shutdown log output in the brief window before
			// termination.
			log.Println("[TRAY] Console close event received, initiating graceful shutdown")
			showConsoleWindow()
			if triggerShutdownFlush != nil {
				triggerShutdownFlush()
			}
			go triggerShutdown()
			return 1
		case CTRL_LOGOFF_EVENT, CTRL_SHUTDOWN_EVENT:
			log.Println("[TRAY] System shutdown/logoff detected, initiating graceful shutdown")
			showConsoleWindow()
			go triggerShutdown()
			return 1
		default:
			return 0
		}
	}

	consoleCtrlHandler = windows.NewCallback(func(a uintptr) uintptr {
		return handler(a)
	})

	ret, _, err := procSetConsoleCtrlHandler.Call(consoleCtrlHandler, 1)
	if ret == 0 {
		log.Printf("[TRAY] Warning: SetConsoleCtrlHandler failed: %v", err)
		return false
	}
	log.Println("[TRAY] Console close interception installed")
	return true
}