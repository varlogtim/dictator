// Package asr wraps sherpa-onnx (ONNX Runtime underneath) for two jobs: the
// Silero voice-activity detector that cuts the microphone stream into
// utterances, and the offline recognizer that turns one utterance into text.
//
// This is the only package in the module that touches cgo. Everything above
// it talks to the small interfaces in package daemon, so the rest of the
// daemon builds and tests without the 30 MB of shared libraries.
package asr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-linux"
)

// SampleRate is what every bundled model expects.
const SampleRate = 16000

// Model types understood by Load. Empty means auto-detect from the files in
// ModelDir.
const (
	ModelNemoTransducer = "nemo_transducer" // Parakeet TDT (encoder/decoder/joiner)
	ModelNemoCTC        = "nemo_ctc"        // Parakeet TDT-CTC 110m and friends (single model file)
)

// VADConfig tunes utterance segmentation. Durations are in seconds.
type VADConfig struct {
	Threshold  float32 // speech probability above which a frame is speech
	MinSilence float32 // silence that ends an utterance
	MinSpeech  float32 // shorter blips are discarded
	MaxSpeech  float32 // force a cut in a monologue with no pauses
}

// DefaultVAD is tuned for dictation: a natural phrase pause ends an utterance.
var DefaultVAD = VADConfig{Threshold: 0.5, MinSilence: 0.4, MinSpeech: 0.25, MaxSpeech: 15}

// Config selects the models.
type Config struct {
	ModelDir  string
	ModelType string
	VADModel  string // path to silero_vad.onnx
	Threads   int    // ONNX Runtime intra-op threads for the recognizer
	VAD       VADConfig
}

// Engine is a loaded recognizer. It is safe for concurrent use; decodes are
// serialized so utterances come out in the order they went in even across a
// session switch, and so a burst never uses more than Threads cores.
type Engine struct {
	cfg  Config
	rec  *sherpa.OfflineRecognizer
	info string
	mu   sync.Mutex
}

// Load reads the models. Expect a couple of seconds for the 0.6B Parakeet.
func Load(cfg Config) (*Engine, error) {
	if cfg.Threads <= 0 {
		cfg.Threads = 4
	}
	if cfg.VAD == (VADConfig{}) {
		cfg.VAD = DefaultVAD
	}
	if _, err := os.Stat(cfg.VADModel); err != nil {
		return nil, fmt.Errorf("vad model: %w", err)
	}
	mc, mtype, err := modelConfig(cfg.ModelDir, cfg.ModelType)
	if err != nil {
		return nil, err
	}
	mc.NumThreads = cfg.Threads
	mc.Provider = "cpu"

	rc := sherpa.OfflineRecognizerConfig{}
	rc.FeatConfig.SampleRate = SampleRate
	rc.FeatConfig.FeatureDim = 80
	rc.ModelConfig = mc
	rc.DecodingMethod = "greedy_search"

	start := time.Now()
	rec := sherpa.NewOfflineRecognizer(&rc)
	if rec == nil {
		return nil, fmt.Errorf("sherpa-onnx failed to create recognizer from %s (details on stderr)", cfg.ModelDir)
	}
	cfg.ModelType = mtype
	return &Engine{
		cfg: cfg,
		rec: rec,
		info: fmt.Sprintf("%s (%s) loaded in %s; sherpa-onnx %s, onnxruntime %s, %d threads",
			filepath.Base(cfg.ModelDir), mtype, time.Since(start).Round(10*time.Millisecond),
			sherpa.GetVersion(), sherpa.GetOnnxruntimeVersion(), cfg.Threads),
	}, nil
}

// Info describes the loaded model for logs.
func (e *Engine) Info() string { return e.info }

// Close frees the recognizer.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rec != nil {
		sherpa.DeleteOfflineRecognizer(e.rec)
		e.rec = nil
	}
}

// Transcribe decodes one utterance of 16 kHz mono float32 samples. It returns
// "" for silence, noise, or output with no letters or digits (a lone "." on a
// breath, for example).
func (e *Engine) Transcribe(samples []float32) (string, error) {
	if len(samples) == 0 {
		return "", nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rec == nil {
		return "", errors.New("asr: engine closed")
	}
	s := sherpa.NewOfflineStream(e.rec)
	defer sherpa.DeleteOfflineStream(s)
	s.AcceptWaveform(SampleRate, samples)
	e.rec.Decode(s)
	text := strings.TrimSpace(s.GetResult().Text)
	if !strings.ContainsFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return "", nil
	}
	return text, nil
}

// Segmenter cuts a stream of samples into utterances with Silero VAD. One
// per session; not safe for concurrent use.
type Segmenter struct {
	vad *sherpa.VoiceActivityDetector
}

// NewSegmenter creates a fresh VAD instance (cheap: the model is ~640 KB).
func (e *Engine) NewSegmenter() (*Segmenter, error) {
	c := sherpa.VadModelConfig{}
	c.SileroVad.Model = e.cfg.VADModel
	c.SileroVad.Threshold = e.cfg.VAD.Threshold
	c.SileroVad.MinSilenceDuration = e.cfg.VAD.MinSilence
	c.SileroVad.MinSpeechDuration = e.cfg.VAD.MinSpeech
	c.SileroVad.MaxSpeechDuration = e.cfg.VAD.MaxSpeech
	c.SileroVad.WindowSize = 512
	c.SampleRate = SampleRate
	c.NumThreads = 1
	c.Provider = "cpu"
	bufSeconds := max(30, 2*e.cfg.VAD.MaxSpeech)
	vad := sherpa.NewVoiceActivityDetector(&c, bufSeconds)
	if vad == nil {
		return nil, fmt.Errorf("sherpa-onnx failed to create VAD from %s (details on stderr)", e.cfg.VADModel)
	}
	return &Segmenter{vad: vad}, nil
}

// Accept feeds samples (any length).
func (s *Segmenter) Accept(samples []float32) {
	if len(samples) > 0 {
		s.vad.AcceptWaveform(samples)
	}
}

// Segments drains utterances that have been closed by silence.
func (s *Segmenter) Segments() [][]float32 {
	var out [][]float32
	for !s.vad.IsEmpty() {
		out = append(out, s.vad.Front().Samples)
		s.vad.Pop()
	}
	return out
}

// Flush finalizes speech still in progress at end of stream and returns it
// along with anything else pending.
func (s *Segmenter) Flush() [][]float32 {
	s.vad.Flush()
	return s.Segments()
}

// Close frees the VAD.
func (s *Segmenter) Close() {
	if s.vad != nil {
		sherpa.DeleteVoiceActivityDetector(s.vad)
		s.vad = nil
	}
}

// modelConfig picks the model files in dir and the sherpa model type.
func modelConfig(dir, want string) (sherpa.OfflineModelConfig, string, error) {
	var mc sherpa.OfflineModelConfig
	if dir == "" {
		return mc, "", errors.New("asr: model dir not set")
	}
	tokens := filepath.Join(dir, "tokens.txt")
	if _, err := os.Stat(tokens); err != nil {
		return mc, "", fmt.Errorf("asr: %w (is %s an extracted sherpa-onnx model directory?)", err, dir)
	}
	mc.Tokens = tokens

	enc, decd, join := pick(dir, "encoder"), pick(dir, "decoder"), pick(dir, "joiner")
	ctc := pick(dir, "model")

	mtype := want
	if mtype == "" {
		switch {
		case enc != "" && decd != "" && join != "":
			mtype = ModelNemoTransducer
		case ctc != "":
			mtype = ModelNemoCTC
		default:
			return mc, "", fmt.Errorf("asr: no encoder/decoder/joiner or model .onnx found in %s", dir)
		}
	}
	switch mtype {
	case ModelNemoTransducer:
		if enc == "" || decd == "" || join == "" {
			return mc, "", fmt.Errorf("asr: %s needs encoder/decoder/joiner .onnx in %s", mtype, dir)
		}
		mc.Transducer.Encoder, mc.Transducer.Decoder, mc.Transducer.Joiner = enc, decd, join
		mc.ModelType = mtype
	case ModelNemoCTC:
		if ctc == "" {
			return mc, "", fmt.Errorf("asr: %s needs model.onnx in %s", mtype, dir)
		}
		mc.NemoCTC.Model = ctc
	default:
		return mc, "", fmt.Errorf("asr: unsupported model type %q (want %s or %s)", mtype, ModelNemoTransducer, ModelNemoCTC)
	}
	return mc, mtype, nil
}

// pick prefers the int8 quantized file, then fp32.
func pick(dir, stem string) string {
	for _, name := range []string{stem + ".int8.onnx", stem + ".onnx"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
