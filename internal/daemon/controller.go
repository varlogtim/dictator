package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/varlogtim/dictator/internal/proto"
	"github.com/varlogtim/dictator/internal/sink"
)

// Config is the controller's static configuration.
type Config struct {
	RecordCmd string // recorder command line (see audio.DefaultCommand)
	NotesDir  string // where "notes" sessions write their files

	TypeCommand string // typing tool, default "wtype"
	TypeSuffix  string // appended after each typed utterance, default " "
	TypeDelayMS int    // per-keystroke delay for the typing tool

	Log *slog.Logger
	Now func() time.Time // injectable clock (tests)
}

// sinkFactory builds the sink for one session of a listening mode. It
// returns the sink and a human-readable detail for the state (e.g. the notes
// file path).
type sinkFactory func(env map[string]string, start time.Time) (sink.Sink, string, error)

// Controller owns the mode. All transitions happen under one mutex, so a
// hotkey mashed twice, a session dying on its own, and a watcher subscribing
// all observe one consistent sequence of states.
type Controller struct {
	cfg Config

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	engine    Engine
	mode      string
	since     time.Time
	detail    string
	active    *session
	subs      map[*subscriber]struct{}
	modeNames []string
	modes     map[string]sinkFactory
}

type subscriber struct {
	ch chan proto.State
}

// New returns a controller in the "starting" state. Call SetEngine once the
// model is loaded to make it "idle" and accept listening modes.
func New(cfg Config) *Controller {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TypeCommand == "" {
		cfg.TypeCommand = "wtype"
	}
	if cfg.TypeSuffix == "" {
		cfg.TypeSuffix = " "
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Controller{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		mode:   proto.ModeStarting,
		since:  cfg.Now(),
		subs:   make(map[*subscriber]struct{}),
		modes:  make(map[string]sinkFactory),
	}
	c.register(proto.ModeType, c.typeSink)
	c.register(proto.ModeNotes, c.notesSink)
	return c
}

// register adds a listening mode. Adding a new kind of output to the daemon
// is one Sink implementation plus one line here.
func (c *Controller) register(name string, f sinkFactory) {
	c.modeNames = append(c.modeNames, name)
	c.modes[name] = f
}

func (c *Controller) typeSink(env map[string]string, _ time.Time) (sink.Sink, string, error) {
	wenv, err := sink.WaylandEnv(env)
	if err != nil {
		return nil, "", err
	}
	s, err := sink.NewType(sink.TypeOptions{
		Command: c.cfg.TypeCommand,
		Suffix:  c.cfg.TypeSuffix,
		DelayMS: c.cfg.TypeDelayMS,
		Env:     wenv,
	})
	if err != nil {
		return nil, "", err
	}
	return s, "", nil
}

func (c *Controller) notesSink(_ map[string]string, start time.Time) (sink.Sink, string, error) {
	n, err := sink.NewNotes(c.cfg.NotesDir, start)
	if err != nil {
		return nil, "", err
	}
	return n, n.Path(), nil
}

// SetEngine installs the loaded speech engine and moves starting → idle.
func (c *Controller) SetEngine(e Engine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.engine = e
	if c.mode == proto.ModeStarting {
		c.setModeLocked(proto.ModeIdle, "")
	}
}

// Status returns the current state.
func (c *Controller) Status() proto.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateLocked()
}

// Set switches to mode: a listening mode or "idle". Switching between two
// listening modes starts the new session before stopping the old one, so a
// failure to start leaves the current session untouched. The old session
// drains asynchronously: its last utterance still goes to the old sink.
func (c *Controller) Set(mode string, env map[string]string) (proto.State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.mode == proto.ModeStarting {
		return c.stateLocked(), errors.New("model still loading")
	}
	if mode == c.mode {
		return c.stateLocked(), nil
	}
	if mode == proto.ModeIdle {
		c.stopActiveLocked()
		c.setModeLocked(proto.ModeIdle, "")
		return c.stateLocked(), nil
	}
	factory, ok := c.modes[mode]
	if !ok {
		return c.stateLocked(), fmt.Errorf("unknown mode %q (have: %s)", mode, strings.Join(c.modeNames, ", "))
	}

	start := c.cfg.Now()
	snk, detail, err := factory(env, start)
	if err != nil {
		return c.stateLocked(), err
	}
	sess, err := startSession(c.ctx, sessionConfig{
		Mode:      mode,
		Sink:      snk,
		Engine:    c.engine,
		RecordCmd: c.cfg.RecordCmd,
		Log:       c.cfg.Log,
	}, c.onSessionExit)
	if err != nil {
		_ = snk.Close()
		return c.stateLocked(), err
	}
	c.stopActiveLocked()
	c.active = sess
	c.setModeLocked(mode, detail)
	return c.stateLocked(), nil
}

// Toggle enters mode, or returns to idle if already in it.
func (c *Controller) Toggle(mode string, env map[string]string) (proto.State, error) {
	c.mu.Lock()
	current := c.mode
	c.mu.Unlock()
	if current == mode {
		return c.Set(proto.ModeIdle, env)
	}
	return c.Set(mode, env)
}

// Subscribe registers a watcher. It returns the state at subscription time
// (taken under the same lock, so no transition can slip between the two)
// and a channel that yields every later state; slow watchers only ever see
// the latest state, never a backlog. Call cancel to unsubscribe.
func (c *Controller) Subscribe() (current proto.State, updates <-chan proto.State, cancel func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &subscriber{ch: make(chan proto.State, 1)}
	c.subs[s] = struct{}{}
	return c.stateLocked(), s.ch, func() {
		c.mu.Lock()
		delete(c.subs, s)
		c.mu.Unlock()
	}
}

// Shutdown stops any session, waiting up to timeout for its last utterance
// to be delivered, then kills whatever is left.
func (c *Controller) Shutdown(timeout time.Duration) {
	c.mu.Lock()
	s := c.active
	c.active = nil
	if c.mode != proto.ModeStarting {
		c.setModeLocked(proto.ModeIdle, "shutting down")
	}
	c.mu.Unlock()

	if s != nil {
		s.Stop()
		select {
		case <-s.Done():
		case <-time.After(timeout):
			c.cfg.Log.Warn("session did not drain in time; killing recorder", "mode", s.mode)
		}
	}
	c.cancel()
}

// onSessionExit is called by a session's pipeline when it has fully ended.
// If it is still the active session, the daemon drops back to idle — the
// recorder died, or the sink kept failing — and says why.
func (c *Controller) onSessionExit(s *session, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != s {
		return // replaced or stopped on purpose
	}
	c.active = nil
	detail := "session ended"
	if err != nil {
		detail = err.Error()
		c.cfg.Log.Error("session ended with error", "mode", s.mode, "err", err)
	} else {
		c.cfg.Log.Info("session ended", "mode", s.mode)
	}
	c.setModeLocked(proto.ModeIdle, detail)
}

func (c *Controller) stopActiveLocked() {
	if c.active != nil {
		c.active.Stop()
		c.active = nil
	}
}

func (c *Controller) setModeLocked(mode, detail string) {
	c.cfg.Log.Info("mode", "from", c.mode, "to", mode, "detail", detail)
	c.mode = mode
	c.since = c.cfg.Now()
	c.detail = detail
	st := c.stateLocked()
	for s := range c.subs {
		// Latest-wins: replace an unread state rather than block.
		select {
		case s.ch <- st:
		default:
			select {
			case <-s.ch:
			default:
			}
			s.ch <- st
		}
	}
}

func (c *Controller) stateLocked() proto.State {
	return proto.State{
		Mode:     c.mode,
		Since:    c.since.Format(time.RFC3339),
		Detail:   c.detail,
		Modes:    append([]string(nil), c.modeNames...),
		Warnings: c.warnings(),
	}
}

// warnings reports missing external tools. Checked live rather than once at
// startup so installing wtype later clears the warning without a restart.
func (c *Controller) warnings() []string {
	var w []string
	if argv := strings.Fields(c.cfg.RecordCmd); len(argv) > 0 {
		if _, err := exec.LookPath(argv[0]); err != nil {
			w = append(w, fmt.Sprintf("recorder %q not found in PATH", argv[0]))
		}
	}
	if _, err := exec.LookPath(c.cfg.TypeCommand); err != nil {
		w = append(w, fmt.Sprintf("%s not found in PATH (Ubuntu: sudo apt install wtype); \"type\" mode unavailable", c.cfg.TypeCommand))
	}
	return w
}
