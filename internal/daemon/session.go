package daemon

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/varlogtim/dictator/internal/audio"
	"github.com/varlogtim/dictator/internal/sink"
)

// sessionConfig describes one listening session.
type sessionConfig struct {
	Mode      string
	Sink      sink.Sink
	Engine    Engine
	RecordCmd string
	Log       *slog.Logger
}

// session is one run of: recorder → segmenter → recognizer → sink.
//
// Lifecycle: startSession launches the recorder and two goroutines. Stop asks
// the recorder to finish; EOF then flows down the pipeline so the utterance
// in progress is still decoded and delivered before the sink is closed. The
// onExit callback fires exactly once, after the sink is closed, with the
// reason the session ended (nil for a requested stop or a recorder that
// simply finished).
type session struct {
	mode string
	rec  *audio.Recorder
	done chan struct{}
}

// maxSinkFailures ends a session whose sink keeps failing (e.g. the typing
// tool cannot reach the compositor) instead of silently eating speech; the
// controller surfaces the error in the state detail.
const maxSinkFailures = 3

func startSession(ctx context.Context, cfg sessionConfig, onExit func(*session, error)) (*session, error) {
	if cfg.Engine == nil {
		return nil, errors.New("session: no engine")
	}
	seg, err := cfg.Engine.NewSegmenter()
	if err != nil {
		return nil, err
	}
	rec, err := audio.Start(ctx, cfg.RecordCmd, nil)
	if err != nil {
		seg.Close()
		return nil, err
	}
	s := &session{mode: cfg.Mode, rec: rec, done: make(chan struct{})}

	// Buffered so a long decode never stalls the recorder read loop; a full
	// pipe would make PipeWire drop audio.
	segCh := make(chan []float32, 32)
	var recErr error // written by pump before close(segCh); read by decode after range ends

	pump := func() {
		defer close(segCh)
		defer seg.Close()
		buf := make([]byte, audio.SampleRate/10*audio.BytesPerSample) // 100 ms
		for {
			n, err := io.ReadFull(rec, buf)
			if n > 0 {
				seg.Accept(pcmToFloat32(buf[:n&^1]))
				for _, u := range seg.Segments() {
					segCh <- u
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
					cfg.Log.Warn("recorder read error", "mode", cfg.Mode, "err", err)
				}
				break
			}
		}
		for _, u := range seg.Flush() {
			segCh <- u
		}
		recErr = rec.Wait()
	}

	decode := func() {
		var sinkErr error
		failures := 0
		for u := range segCh {
			if sinkErr != nil {
				continue // draining; the recorder has been told to stop
			}
			text, err := cfg.Engine.Transcribe(u)
			if err != nil {
				cfg.Log.Error("transcribe failed", "mode", cfg.Mode, "err", err)
				continue
			}
			if text == "" {
				continue
			}
			cfg.Log.Debug("utterance", "mode", cfg.Mode, "seconds", float64(len(u))/audio.SampleRate, "text", text)
			if err := cfg.Sink.Write(text); err != nil {
				failures++
				cfg.Log.Error("sink write failed", "mode", cfg.Mode, "attempt", failures, "err", err)
				if failures >= maxSinkFailures {
					sinkErr = fmt.Errorf("%s output failing: %w", cfg.Mode, err)
					rec.Stop() // let the pipeline drain and end
				}
				continue
			}
			failures = 0
		}
		if err := cfg.Sink.Close(); err != nil {
			cfg.Log.Warn("sink close failed", "mode", cfg.Mode, "err", err)
		}
		exitErr := sinkErr
		if exitErr == nil && recErr != nil {
			exitErr = fmt.Errorf("recorder: %w", recErr)
		}
		onExit(s, exitErr)
		close(s.done)
	}

	go pump()
	go decode()
	return s, nil
}

// Stop asks the session to finish. It returns immediately; Done is closed
// once the last utterance has been delivered and the sink closed.
func (s *session) Stop() { s.rec.Stop() }

// Done is closed when the session has fully ended.
func (s *session) Done() <-chan struct{} { return s.done }

// pcmToFloat32 converts little-endian signed 16-bit PCM to [-1, 1).
func pcmToFloat32(b []byte) []float32 {
	out := make([]float32, len(b)/2)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(b[2*i:]))) / 32768
	}
	return out
}
