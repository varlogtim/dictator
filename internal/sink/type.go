package sink

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// TypeOptions configures a Type sink.
type TypeOptions struct {
	// Command is the typing tool; only wtype's CLI is supported ("-d", "--").
	Command string
	// Suffix is appended to every utterance so consecutive phrases don't run
	// together. Default " ".
	Suffix string
	// DelayMS is passed as `-d` (per-keystroke delay). Some apps drop
	// synthetic input that arrives too quickly; 0 disables.
	DelayMS int
	// Env is KEY=VALUE pairs appended to the tool's environment; must carry
	// WAYLAND_DISPLAY (see WaylandEnv).
	Env []string
	// Timeout bounds one invocation. Default 5s.
	Timeout time.Duration
}

// Type sends each utterance as keystrokes to the focused window by running
// wtype once per utterance. wtype reads all of its input before typing, so a
// process per utterance is the natural unit; the ~10 ms spawn cost is far
// below speech pacing.
type Type struct {
	opt TypeOptions
}

// NewType validates that the typing tool is installed and returns the sink.
func NewType(opt TypeOptions) (*Type, error) {
	if opt.Command == "" {
		opt.Command = "wtype"
	}
	if opt.Suffix == "" {
		opt.Suffix = " "
	}
	if opt.Timeout == 0 {
		opt.Timeout = 5 * time.Second
	}
	if _, err := exec.LookPath(opt.Command); err != nil {
		return nil, fmt.Errorf("%s not found in PATH (Ubuntu: sudo apt install wtype)", opt.Command)
	}
	return &Type{opt: opt}, nil
}

// Write types one utterance followed by the suffix.
func (t *Type) Write(text string) error {
	// wtype maps '\n' to Return; an utterance must never press keys other
	// than its characters, so flatten any whitespace runs.
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.opt.Timeout)
	defer cancel()

	var args []string
	if t.opt.DelayMS > 0 {
		args = append(args, "-d", strconv.Itoa(t.opt.DelayMS))
	}
	args = append(args, "--", text+t.opt.Suffix)

	cmd := exec.CommandContext(ctx, t.opt.Command, args...)
	cmd.Env = append(os.Environ(), t.opt.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", t.opt.Command, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Close is a no-op; nothing is held between utterances.
func (t *Type) Close() error { return nil }

// WaylandEnv builds the environment the typing tool needs from the client's
// forwarded environment, falling back to the daemon's own environment and
// finally to the first wayland-* socket under XDG_RUNTIME_DIR (the daemon is
// a user service and may have started before the compositor, so it cannot
// rely on inheriting WAYLAND_DISPLAY).
func WaylandEnv(clientEnv map[string]string) ([]string, error) {
	get := func(k string) string {
		if v := clientEnv[k]; v != "" {
			return v
		}
		return os.Getenv(k)
	}
	runtimeDir := get("XDG_RUNTIME_DIR")
	display := get("WAYLAND_DISPLAY")
	if display == "" && runtimeDir != "" {
		matches, _ := filepath.Glob(filepath.Join(runtimeDir, "wayland-*"))
		for _, m := range matches {
			if !strings.HasSuffix(m, ".lock") {
				display = filepath.Base(m)
				break
			}
		}
	}
	if display == "" {
		return nil, fmt.Errorf("cannot determine WAYLAND_DISPLAY (not in client env, daemon env, or %s)", runtimeDir)
	}
	env := []string{"WAYLAND_DISPLAY=" + display}
	if runtimeDir != "" {
		env = append(env, "XDG_RUNTIME_DIR="+runtimeDir)
	}
	for _, k := range []string{"DISPLAY", "SWAYSOCK"} {
		if v := clientEnv[k]; v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env, nil
}
