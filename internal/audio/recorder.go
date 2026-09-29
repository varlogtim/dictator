// Package audio captures microphone audio by running an external recorder
// command and exposing its stdout as an io.Reader of raw PCM.
//
// Why exec a tool instead of binding PipeWire directly: it keeps this package
// cgo-free, it makes the capture stage swappable from the command line
// (pw-record, parecord, arecord, a PipeWire filter-chain, or `cat file.raw`
// in tests), and it gives a clean shutdown story for free — terminating the
// recorder produces EOF on the pipe, which lets the consumer flush whatever
// speech is still buffered before it stops.
package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Format the recorder is expected to emit.
const (
	SampleRate     = 16000
	BytesPerSample = 2 // s16le
)

// DefaultCommand records 16 kHz mono s16le to stdout via PipeWire. pw-record
// writes headerless PCM when the target is "-".
const DefaultCommand = "pw-record --rate 16000 --channels 1 --format s16 -"

// Recorder is a running recorder process. Read the PCM from it; call Stop to
// ask it to finish; keep reading until io.EOF; then call Wait.
type Recorder struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser

	stopReq  atomic.Bool
	stopOnce sync.Once
	waitOnce sync.Once
	waitErr  error

	mu      sync.Mutex
	killTmr *time.Timer
}

// Start launches the recorder command (split on whitespace) with the given
// environment appended to the current one.
func Start(ctx context.Context, command string, extraEnv []string) (*Recorder, error) {
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return nil, errors.New("audio: empty recorder command")
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return nil, fmt.Errorf("audio: recorder %q not found in PATH", argv[0])
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("audio: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("audio: start %q: %w", argv[0], err)
	}
	return &Recorder{cmd: cmd, stdout: stdout}, nil
}

// Read implements io.Reader over the recorder's stdout.
func (r *Recorder) Read(p []byte) (int, error) { return r.stdout.Read(p) }

// Stop asks the recorder to finish (SIGTERM) and arms a SIGKILL fallback.
// It returns immediately; the reader will observe io.EOF shortly after.
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() {
		r.stopReq.Store(true)
		_ = r.cmd.Process.Signal(syscall.SIGTERM)
		r.mu.Lock()
		r.killTmr = time.AfterFunc(2*time.Second, func() { _ = r.cmd.Process.Kill() })
		r.mu.Unlock()
	})
}

// Wait reaps the process. Call it only after Read returned io.EOF (Wait
// closes the pipe). A recorder that was told to Stop and then died from the
// signal is reported as a clean exit.
func (r *Recorder) Wait() error {
	r.waitOnce.Do(func() {
		err := r.cmd.Wait()
		r.mu.Lock()
		if r.killTmr != nil {
			r.killTmr.Stop()
		}
		r.mu.Unlock()
		if err != nil && r.stopReq.Load() && signaled(err) {
			err = nil
		}
		r.waitErr = err
	})
	return r.waitErr
}

func signaled(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}
