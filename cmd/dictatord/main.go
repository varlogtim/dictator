// dictatord is the speech-to-text daemon: it keeps the model warm, listens
// on a Unix socket for mode changes, and while in a listening mode runs
// microphone → VAD → recognizer → sink.
//
// Configuration is flags; every flag has a DICTATOR_* environment variable
// fallback so a systemd EnvironmentFile can set it without editing the unit.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/varlogtim/dictator/internal/asr"
	"github.com/varlogtim/dictator/internal/audio"
	"github.com/varlogtim/dictator/internal/daemon"
	"github.com/varlogtim/dictator/internal/proto"
)

var version = "dev" // set by -ldflags "-X main.version=..."

// DefaultModelName is the model `make models` installs.
const DefaultModelName = "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8"

func main() {
	os.Exit(run())
}

func run() int {
	dataDir := defaultDataDir()
	home, _ := os.UserHomeDir()

	var (
		socketPath = flag.String("socket", envOr("DICTATOR_SOCKET", proto.DefaultSocketPath()), "unix socket path")
		modelDir   = flag.String("model-dir", envOr("DICTATOR_MODEL_DIR", filepath.Join(dataDir, "models", DefaultModelName)), "extracted sherpa-onnx model directory")
		modelType  = flag.String("model-type", envOr("DICTATOR_MODEL_TYPE", ""), "nemo_transducer|nemo_ctc (default: detect from files)")
		vadModel   = flag.String("vad-model", envOr("DICTATOR_VAD_MODEL", filepath.Join(dataDir, "models", "silero_vad.onnx")), "silero VAD model file")
		threads    = flag.Int("threads", envInt("DICTATOR_THREADS", 4), "recognizer threads")
		notesDir   = flag.String("notes-dir", envOr("DICTATOR_NOTES_DIR", filepath.Join(home, "notes", "dictation")), "directory for notes-mode files")
		recordCmd  = flag.String("record-cmd", envOr("DICTATOR_RECORD_CMD", audio.DefaultCommand), "command producing 16 kHz mono s16le PCM on stdout")
		typeCmd    = flag.String("type-cmd", envOr("DICTATOR_TYPE_CMD", "wtype"), "typing tool for type mode")
		typeSuffix = flag.String("type-suffix", envOr("DICTATOR_TYPE_SUFFIX", " "), "text typed after every utterance")
		typeDelay  = flag.Int("type-delay-ms", envInt("DICTATOR_TYPE_DELAY_MS", 0), "per-keystroke delay for the typing tool (0 = none)")
		vadThresh  = flag.Float64("vad-threshold", envFloat("DICTATOR_VAD_THRESHOLD", float64(asr.DefaultVAD.Threshold)), "speech probability threshold")
		vadSilence = flag.Float64("vad-min-silence", envFloat("DICTATOR_VAD_MIN_SILENCE", float64(asr.DefaultVAD.MinSilence)), "seconds of silence that end an utterance")
		vadSpeech  = flag.Float64("vad-min-speech", envFloat("DICTATOR_VAD_MIN_SPEECH", float64(asr.DefaultVAD.MinSpeech)), "seconds; shorter sounds are ignored")
		vadMax     = flag.Float64("vad-max-speech", envFloat("DICTATOR_VAD_MAX_SPEECH", float64(asr.DefaultVAD.MaxSpeech)), "seconds; force a cut in continuous speech")
		logLevel   = flag.String("log-level", envOr("DICTATOR_LOG_LEVEL", "info"), "debug|info|warn|error")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("dictatord", version)
		return 0
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "bad -log-level %q\n", *logLevel)
		return 2
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	log.Info("dictatord starting", "version", version, "socket", *socketPath, "model", *modelDir, "notes", *notesDir)

	ctrl := daemon.New(daemon.Config{
		RecordCmd:   *recordCmd,
		NotesDir:    *notesDir,
		TypeCommand: *typeCmd,
		TypeSuffix:  *typeSuffix,
		TypeDelayMS: *typeDelay,
		Log:         log,
	})
	for _, w := range ctrl.Status().Warnings {
		log.Warn(w)
	}

	// Listen before loading the model so clients and the indicator see
	// "starting" instead of a connection refused during the load.
	srv, err := daemon.Listen(*socketPath, ctrl, log)
	if err != nil {
		log.Error("listen failed", "err", err)
		return 1
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	engine, err := asr.Load(asr.Config{
		ModelDir:  *modelDir,
		ModelType: *modelType,
		VADModel:  *vadModel,
		Threads:   *threads,
		VAD: asr.VADConfig{
			Threshold:  float32(*vadThresh),
			MinSilence: float32(*vadSilence),
			MinSpeech:  float32(*vadSpeech),
			MaxSpeech:  float32(*vadMax),
		},
	})
	if err != nil {
		log.Error("model load failed", "err", err, "hint", "run `make models` or set DICTATOR_MODEL_DIR")
		srv.Close()
		return 1
	}
	log.Info("model ready", "info", engine.Info())
	ctrl.SetEngine(engineAdapter{engine})

	select {
	case sig := <-sigs:
		log.Info("shutting down", "signal", sig)
	case err := <-serveErr:
		log.Error("server stopped", "err", err)
	}
	ctrl.Shutdown(3 * time.Second)
	srv.Close()
	engine.Close()
	return 0
}

// engineAdapter bridges asr.Engine (concrete *Segmenter) to daemon.Engine.
type engineAdapter struct{ *asr.Engine }

func (a engineAdapter) NewSegmenter() (daemon.Segmenter, error) { return a.Engine.NewSegmenter() }

func defaultDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "dictator")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "dictator")
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
