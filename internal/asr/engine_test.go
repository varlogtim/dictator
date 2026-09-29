package asr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-linux"
)

// Integration test against real models. Skipped unless both are provided:
//
//	DICTATOR_TEST_MODEL_DIR=.../sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8 \
//	DICTATOR_TEST_VAD_MODEL=.../silero_vad.onnx go test ./internal/asr/
func loadTestEngine(t *testing.T) *Engine {
	t.Helper()
	dir, vad := os.Getenv("DICTATOR_TEST_MODEL_DIR"), os.Getenv("DICTATOR_TEST_VAD_MODEL")
	if dir == "" || vad == "" {
		t.Skip("set DICTATOR_TEST_MODEL_DIR and DICTATOR_TEST_VAD_MODEL to run")
	}
	e, err := Load(Config{ModelDir: dir, VADModel: vad, Threads: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	t.Log(e.Info())
	return e
}

func testWave(t *testing.T) []float32 {
	t.Helper()
	p := filepath.Join(os.Getenv("DICTATOR_TEST_MODEL_DIR"), "test_wavs", "0.wav")
	w := sherpa.ReadWave(p)
	if w == nil || len(w.Samples) == 0 {
		t.Fatalf("could not read %s", p)
	}
	if w.SampleRate != SampleRate {
		t.Fatalf("test wav is %d Hz, want %d", w.SampleRate, SampleRate)
	}
	return w.Samples
}

func TestTranscribeKnownUtterance(t *testing.T) {
	e := loadTestEngine(t)
	text, err := e.Transcribe(testWave(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("text: %q", text)
	if !strings.Contains(text, "portrait") {
		t.Fatalf("unexpected transcript %q", text)
	}
	if !strings.HasSuffix(strings.TrimSpace(text), ".") {
		t.Errorf("expected punctuation from this model, got %q", text)
	}
}

func TestTranscribeSilenceIsEmpty(t *testing.T) {
	e := loadTestEngine(t)
	text, err := e.Transcribe(make([]float32, 2*SampleRate))
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Fatalf("silence produced %q", text)
	}
}

func TestSegmenterCutsOnSilence(t *testing.T) {
	e := loadTestEngine(t)
	seg, err := e.NewSegmenter()
	if err != nil {
		t.Fatal(err)
	}
	defer seg.Close()

	speech := testWave(t)
	silence := make([]float32, SampleRate) // 1 s
	stream := append(append(append(append([]float32{}, silence...), speech...), silence...), speech...)
	stream = append(stream, silence...)

	var segs [][]float32
	const chunk = SampleRate / 10
	for i := 0; i < len(stream); i += chunk {
		seg.Accept(stream[i:min(i+chunk, len(stream))])
		segs = append(segs, seg.Segments()...)
	}
	segs = append(segs, seg.Flush()...)
	if len(segs) < 2 {
		t.Fatalf("got %d segments, want at least 2 (one per spoken passage)", len(segs))
	}
	var texts []string
	for _, s := range segs {
		txt, err := e.Transcribe(s)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, txt)
	}
	t.Logf("segments: %d, texts: %q", len(segs), texts)
	if n := strings.Count(strings.Join(texts, " "), "portrait"); n != 2 {
		t.Fatalf("expected the passage twice, got %d in %q", n, texts)
	}
}
