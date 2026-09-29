// Package proto defines the wire protocol between the dictator client and
// dictatord, plus the shared vocabulary (mode names, socket location).
//
// The protocol is deliberately tiny: newline-delimited JSON over a Unix
// socket. One Request line in, one Response line out — except for "watch",
// where the daemon keeps the connection open and writes a Response line every
// time the state changes. It is debuggable by hand:
//
//	printf '{"cmd":"status"}\n' | nc -U "$XDG_RUNTIME_DIR/dictator.sock"
package proto

import (
	"fmt"
	"os"
	"path/filepath"
)

// Mode names. "starting" and "idle" are daemon-internal states; the listening
// modes ("type", "notes") are the ones a client can request.
const (
	ModeStarting = "starting" // daemon is up, model still loading
	ModeIdle     = "idle"     // model loaded, microphone released
	ModeType     = "type"     // transcribe and type into the focused window
	ModeNotes    = "notes"    // transcribe and append to a per-session notes file
)

// Commands understood by the daemon.
const (
	CmdStatus = "status" // reply with the current State
	CmdSet    = "set"    // switch to Mode (a listening mode or "idle")
	CmdToggle = "toggle" // Mode if not already in it, otherwise "idle"
	CmdWatch  = "watch"  // stream State on every change until the client hangs up
)

// Request is one client command.
type Request struct {
	Cmd  string `json:"cmd"`
	Mode string `json:"mode,omitempty"`
	// Env carries a whitelisted slice of the *client's* environment
	// (WAYLAND_DISPLAY etc.). The daemon is a systemd user service that may
	// have started before the compositor, so the process that actually knows
	// which Wayland display to type into is the client — it runs from the
	// hotkey inside the session.
	Env map[string]string `json:"env,omitempty"`
}

// Response is one daemon reply (or one state event on a watch stream).
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	State *State `json:"state,omitempty"`
}

// State is the daemon's externally visible state.
type State struct {
	Mode  string `json:"mode"`
	Since string `json:"since"` // RFC3339, when Mode was entered
	// Detail is mode-specific human-readable context: the notes file path in
	// "notes" mode, or the reason for an unexpected return to "idle".
	Detail string `json:"detail,omitempty"`
	// Modes lists the listening modes this daemon offers.
	Modes []string `json:"modes"`
	// Warnings lists degraded-environment conditions (e.g. wtype not
	// installed) so an indicator can surface them.
	Warnings []string `json:"warnings,omitempty"`
}

// EnvKeys is the whitelist of client environment variables forwarded to the
// daemon and injected into the environment of the typing tool.
var EnvKeys = []string{"WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DISPLAY", "SWAYSOCK"}

// ClientEnv collects EnvKeys from the current process environment.
func ClientEnv() map[string]string {
	env := make(map[string]string, len(EnvKeys))
	for _, k := range EnvKeys {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env[k] = v
		}
	}
	return env
}

// DefaultSocketPath returns $DICTATOR_SOCKET if set, else
// $XDG_RUNTIME_DIR/dictator.sock, else a per-uid path under /tmp.
func DefaultSocketPath() string {
	if p := os.Getenv("DICTATOR_SOCKET"); p != "" {
		return p
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "dictator.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("dictator-%d.sock", os.Getuid()))
}
