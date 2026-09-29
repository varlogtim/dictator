// Package sink delivers transcribed utterances somewhere.
//
// A Sink receives one utterance at a time, in order, on a single goroutine,
// and is closed exactly once when its session ends. Two sinks exist today
// (type into the focused window; append to a notes file). Adding a mode to
// the daemon means adding a Sink here and registering a factory for it.
package sink

// Sink consumes utterances for the lifetime of one listening session.
type Sink interface {
	// Write delivers one utterance (already trimmed, non-empty).
	Write(text string) error
	// Close releases resources. It is called once, after the last Write.
	Close() error
}
