package sink

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotesWritesOneLinePerUtterance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "notes")
	start := time.Date(2026, 9, 29, 9, 41, 5, 0, time.Local)
	n, err := NewNotes(dir, start)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Base(n.Path()), "2026-09-29_09-41-05.md"; got != want {
		t.Fatalf("filename %q, want %q", got, want)
	}
	for _, u := range []string{"First thought.", "Second, with a comma."} {
		if err := n.Write(u); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(n.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "First thought.\nSecond, with a comma.\n"; got != want {
		t.Fatalf("content %q, want %q", got, want)
	}
}

func TestNotesRemovesEmptyFileOnClose(t *testing.T) {
	dir := t.TempDir()
	n, err := NewNotes(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(n.Path()); !os.IsNotExist(err) {
		t.Fatalf("empty notes file should have been removed, stat err=%v", err)
	}
}

// fakeWtype installs a script named wtype on PATH that appends its argv,
// one arg per line, to log; it returns the log path.
func fakeWtype(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls.log")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> \"" + log + "\"; done\nprintf -- '--\\n' >> \"" + log + "\"\n"
	if err := os.WriteFile(filepath.Join(bin, "wtype"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestTypeInvokesWtypePerUtteranceWithSuffix(t *testing.T) {
	log := fakeWtype(t)
	s, err := NewType(TypeOptions{DelayMS: 3, Env: []string{"WAYLAND_DISPLAY=wayland-9"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write("Hello, world."); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("line\nbreaks  collapse"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "-d\n3\n--\nHello, world. \n--\n-d\n3\n--\nline breaks collapse \n--\n"
	if string(b) != want {
		t.Fatalf("wtype argv log:\n%q\nwant:\n%q", string(b), want)
	}
}

func TestTypeMissingToolIsAClearError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := NewType(TypeOptions{})
	if err == nil || !strings.Contains(err.Error(), "apt install wtype") {
		t.Fatalf("want install hint, got %v", err)
	}
}

func TestWaylandEnvPrefersClientThenGlob(t *testing.T) {
	rt := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", rt)
	t.Setenv("WAYLAND_DISPLAY", "")

	env, err := WaylandEnv(map[string]string{"WAYLAND_DISPLAY": "wayland-7", "SWAYSOCK": "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(env, "WAYLAND_DISPLAY=wayland-7") || !contains(env, "SWAYSOCK=/x") || !contains(env, "XDG_RUNTIME_DIR="+rt) {
		t.Fatalf("client env not honoured: %v", env)
	}

	if _, err := WaylandEnv(nil); err == nil {
		t.Fatal("expected error with no display anywhere")
	}

	for _, f := range []string{"wayland-1.lock", "wayland-1"} {
		if err := os.WriteFile(filepath.Join(rt, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env, err = WaylandEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(env, "WAYLAND_DISPLAY=wayland-1") {
		t.Fatalf("glob fallback failed: %v", env)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
