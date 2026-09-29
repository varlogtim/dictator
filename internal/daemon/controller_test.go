package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/varlogtim/dictator/internal/proto"
)

// fakeEngine emits one "utterance" per utteranceSamples samples and
// transcribes it to a numbered string, so tests can count and order output
// without any model.
type fakeEngine struct {
	utteranceSamples int
	mu               sync.Mutex
	transcribed      int
	transcribeText   func(n int, samples []float32) string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		utteranceSamples: 16000, // 1 s of audio per utterance
		transcribeText:   func(n int, _ []float32) string { return fmt.Sprintf("utterance %d.", n) },
	}
}

func (e *fakeEngine) NewSegmenter() (Segmenter, error) { return &fakeSegmenter{e: e}, nil }

func (e *fakeEngine) Transcribe(samples []float32) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.transcribed++
	return e.transcribeText(e.transcribed, samples), nil
}

type fakeSegmenter struct {
	e     *fakeEngine
	buf   []float32
	ready [][]float32
}

func (s *fakeSegmenter) Accept(samples []float32) {
	s.buf = append(s.buf, samples...)
	for len(s.buf) >= s.e.utteranceSamples {
		s.ready = append(s.ready, s.buf[:s.e.utteranceSamples])
		s.buf = s.buf[s.e.utteranceSamples:]
	}
}
func (s *fakeSegmenter) Segments() [][]float32 { r := s.ready; s.ready = nil; return r }
func (s *fakeSegmenter) Flush() [][]float32 {
	if len(s.buf) > 0 {
		s.ready = append(s.ready, s.buf)
		s.buf = nil
	}
	return s.Segments()
}
func (s *fakeSegmenter) Close() {}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// fakeWtype puts a wtype stand-in on PATH that logs argv; returns the log.
func fakeWtype(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$2\" >> \"" + log + "\"\n" // $1 is "--"
	if err := os.WriteFile(filepath.Join(bin, "wtype"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// realtimeRecorder returns the path of a script that emits 16 kHz s16le
// silence at real-time pace (100 ms chunks) and exits promptly on SIGTERM
// without leaving a child holding the pipe open, so EOF is immediate.
func realtimeRecorder(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rt-zero")
	script := `#!/bin/sh
trap 'exit 0' TERM INT
while :; do
  head -c 3200 /dev/zero
  sleep 0.1 >/dev/null & wait $!
done
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newReadyController(t *testing.T, recordCmd string) (*Controller, *fakeEngine) {
	t.Helper()
	eng := newFakeEngine()
	c := New(Config{
		RecordCmd: recordCmd,
		NotesDir:  filepath.Join(t.TempDir(), "notes"),
		Log:       testLogger(t),
	})
	c.SetEngine(eng)
	t.Cleanup(func() { c.Shutdown(3 * time.Second) })
	return c, eng
}

// waitForMode blocks until the controller reports mode (via a watcher).
func waitForMode(t *testing.T, c *Controller, mode string) proto.State {
	t.Helper()
	cur, updates, cancel := c.Subscribe()
	defer cancel()
	if cur.Mode == mode {
		return cur
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case st := <-updates:
			if st.Mode == mode {
				return st
			}
		case <-deadline:
			t.Fatalf("timed out waiting for mode %q (now %q)", mode, c.Status().Mode)
		}
	}
}

func TestStartingRejectsListeningModes(t *testing.T) {
	c := New(Config{RecordCmd: "cat /dev/zero", Log: testLogger(t)})
	if st := c.Status(); st.Mode != proto.ModeStarting {
		t.Fatalf("initial mode %q", st.Mode)
	}
	if _, err := c.Set(proto.ModeNotes, nil); err == nil {
		t.Fatal("expected error while starting")
	}
	c.SetEngine(newFakeEngine())
	if st := c.Status(); st.Mode != proto.ModeIdle {
		t.Fatalf("after SetEngine mode %q, want idle", st.Mode)
	}
	if got := c.Status().Modes; strings.Join(got, ",") != "type,notes" {
		t.Fatalf("modes %v", got)
	}
}

func TestNotesSessionWritesEveryUtteranceThenIdlesWhenRecorderEnds(t *testing.T) {
	// 3.5 s of zeros: three full utterances plus a half one the flush delivers.
	c, _ := newReadyController(t, "head -c 112000 /dev/zero")

	st, err := c.Set(proto.ModeNotes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != proto.ModeNotes || !strings.HasSuffix(st.Detail, ".md") {
		t.Fatalf("state after set: %+v", st)
	}
	notesPath := st.Detail

	idle := waitForMode(t, c, proto.ModeIdle)
	if idle.Detail != "session ended" {
		t.Errorf("detail %q, want %q", idle.Detail, "session ended")
	}
	b, err := os.ReadFile(notesPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "utterance 1.\nutterance 2.\nutterance 3.\nutterance 4.\n"
	if string(b) != want {
		t.Fatalf("notes content:\n%q\nwant:\n%q", string(b), want)
	}
}

func TestToggleEntersThenLeavesAndFlushesLastUtterance(t *testing.T) {
	c, _ := newReadyController(t, realtimeRecorder(t))

	st, err := c.Toggle(proto.ModeNotes, nil)
	if err != nil || st.Mode != proto.ModeNotes {
		t.Fatalf("toggle on: %+v %v", st, err)
	}
	notesPath := st.Detail
	// Less than one full fake utterance (1 s) of audio: the only way anything
	// reaches the file is the end-of-stream flush after toggling off.
	time.Sleep(250 * time.Millisecond)

	st, err = c.Toggle(proto.ModeNotes, nil)
	if err != nil || st.Mode != proto.ModeIdle {
		t.Fatalf("toggle off: %+v %v", st, err)
	}
	// The session drains asynchronously.
	deadline := time.Now().Add(5 * time.Second)
	var b []byte
	for time.Now().Before(deadline) {
		b, _ = os.ReadFile(notesPath)
		if len(b) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if string(b) != "utterance 1.\n" {
		t.Fatalf("notes content %q, want the single flushed utterance", string(b))
	}
}

func TestSwitchingModesStopsOldSessionAndStartsNew(t *testing.T) {
	wlog := fakeWtype(t)
	c, eng := newReadyController(t, realtimeRecorder(t))
	eng.utteranceSamples = 1600 // one fake utterance per 100 ms chunk
	env := map[string]string{"WAYLAND_DISPLAY": "wayland-test", "XDG_RUNTIME_DIR": t.TempDir()}

	if st, err := c.Set(proto.ModeType, env); err != nil || st.Mode != proto.ModeType {
		t.Fatalf("set type: %+v %v", st, err)
	}
	time.Sleep(350 * time.Millisecond)
	st, err := c.Set(proto.ModeNotes, env)
	if err != nil || st.Mode != proto.ModeNotes {
		t.Fatalf("set notes: %+v %v", st, err)
	}
	notesPath := st.Detail
	time.Sleep(350 * time.Millisecond)

	// The old session must be gone: its log stops growing.
	typedBefore, _ := os.ReadFile(wlog)
	time.Sleep(300 * time.Millisecond)
	typedAfter, _ := os.ReadFile(wlog)
	if string(typedBefore) != string(typedAfter) {
		t.Fatalf("type session still producing after switch:\n%q\n%q", typedBefore, typedAfter)
	}

	if st, err := c.Set(proto.ModeIdle, nil); err != nil || st.Mode != proto.ModeIdle {
		t.Fatalf("set idle: %+v %v", st, err)
	}
	c.Shutdown(3 * time.Second)

	noted, _ := os.ReadFile(notesPath)
	typedN, notedN := utteranceNumbers(t, string(typedAfter)), utteranceNumbers(t, string(noted))
	if len(typedN) < 2 || len(notedN) < 2 {
		t.Fatalf("expected several utterances in both sinks; typed=%v noted=%v", typedN, notedN)
	}
	// Within a sink, delivery order is decode order.
	for _, seq := range [][]int{typedN, notedN} {
		for i := 1; i < len(seq); i++ {
			if seq[i] <= seq[i-1] {
				t.Fatalf("out-of-order delivery: %v", seq)
			}
		}
	}
}

// utteranceNumbers parses "utterance N." lines from a sink log.
func utteranceNumbers(t *testing.T, s string) []int {
	t.Helper()
	var out []int
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(line, "utterance %d.", &n); err != nil {
			t.Fatalf("unexpected line %q", line)
		}
		out = append(out, n)
	}
	return out
}

func TestSetUnknownModeAndSetSameModeAreHandled(t *testing.T) {
	c, _ := newReadyController(t, realtimeRecorder(t))
	if _, err := c.Set("shout", nil); err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Fatalf("want unknown mode error, got %v", err)
	}
	if st, err := c.Set(proto.ModeIdle, nil); err != nil || st.Mode != proto.ModeIdle {
		t.Fatalf("idle→idle should be a no-op success: %+v %v", st, err)
	}
}

func TestTypeModeWithoutWaylandDisplayFailsWithoutChangingMode(t *testing.T) {
	fakeWtype(t)
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // empty: no wayland-* to glob
	c, _ := newReadyController(t, realtimeRecorder(t))
	_, err := c.Set(proto.ModeType, nil)
	if err == nil || !strings.Contains(err.Error(), "WAYLAND_DISPLAY") {
		t.Fatalf("want WAYLAND_DISPLAY error, got %v", err)
	}
	if st := c.Status(); st.Mode != proto.ModeIdle {
		t.Fatalf("mode should be unchanged, got %q", st.Mode)
	}
}

func TestFailingSinkEndsSessionWithDetail(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "wtype"), []byte("#!/bin/sh\necho 'compositor gone' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c, _ := newReadyController(t, realtimeRecorder(t))
	env := map[string]string{"WAYLAND_DISPLAY": "wayland-test", "XDG_RUNTIME_DIR": t.TempDir()}
	if _, err := c.Set(proto.ModeType, env); err != nil {
		t.Fatal(err)
	}
	st := waitForMode(t, c, proto.ModeIdle)
	if !strings.Contains(st.Detail, "type output failing") || !strings.Contains(st.Detail, "compositor gone") {
		t.Fatalf("detail %q should explain the sink failure", st.Detail)
	}
}

func TestWarningsReportMissingTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	c := New(Config{RecordCmd: "pw-record -", Log: testLogger(t)})
	w := strings.Join(c.Status().Warnings, "\n")
	if !strings.Contains(w, `recorder "pw-record" not found`) || !strings.Contains(w, "apt install wtype") {
		t.Fatalf("warnings: %q", w)
	}
}

func TestSubscribersGetLatestStateWithoutBlocking(t *testing.T) {
	c, _ := newReadyController(t, realtimeRecorder(t))
	cur, updates, cancel := c.Subscribe()
	defer cancel()
	if cur.Mode != proto.ModeIdle {
		t.Fatalf("current %q", cur.Mode)
	}
	// Two transitions without the watcher reading: only the latest survives.
	if _, err := c.Set(proto.ModeNotes, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Set(proto.ModeIdle, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case st := <-updates:
		if st.Mode != proto.ModeIdle {
			t.Fatalf("got %q, want the latest (idle)", st.Mode)
		}
	case <-time.After(time.Second):
		t.Fatal("no update delivered")
	}
	select {
	case st := <-updates:
		t.Fatalf("unexpected backlog: %+v", st)
	default:
	}
}
