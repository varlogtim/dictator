// Package daemon is the heart of dictatord: a controller that owns the
// current mode, runs at most one listening session at a time, and fans state
// changes out to watchers; plus the Unix-socket server that exposes it.
//
// The speech stack is abstracted behind two tiny interfaces so this package
// (and its tests) build without cgo. Package asr implements them.
package daemon

// Engine is what a session needs from the speech stack.
type Engine interface {
	// NewSegmenter returns a fresh voice-activity detector for one session.
	NewSegmenter() (Segmenter, error)
	// Transcribe decodes one utterance of 16 kHz mono samples. "" means
	// nothing worth delivering.
	Transcribe(samples []float32) (string, error)
}

// Segmenter cuts a sample stream into utterances.
type Segmenter interface {
	Accept(samples []float32)
	// Segments drains utterances that are complete so far.
	Segments() [][]float32
	// Flush ends the stream: whatever speech is in progress becomes a final
	// utterance. It returns all pending utterances.
	Flush() [][]float32
	Close()
}
