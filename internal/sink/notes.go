package sink

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// NotesFilenameLayout names one file per notes session by the time the
// session began.
const NotesFilenameLayout = "2006-01-02_15-04-05"

// Notes appends one utterance per line to a file created for the session.
type Notes struct {
	f    *os.File
	path string
	n    int
}

// NewNotes creates <dir>/<start time>.md (and dir itself if needed).
func NewNotes(dir string, start time.Time) (*Notes, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("notes dir: %w", err)
	}
	path := filepath.Join(dir, start.Format(NotesFilenameLayout)+".md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("notes file: %w", err)
	}
	return &Notes{f: f, path: path}, nil
}

// Path is the notes file for this session.
func (n *Notes) Path() string { return n.path }

// Write appends the utterance as its own line. *os.File writes are
// unbuffered, so a line is visible to `tail -f` as soon as it is decoded.
func (n *Notes) Write(text string) error {
	if _, err := n.f.WriteString(text + "\n"); err != nil {
		return err
	}
	n.n++
	return nil
}

// Close closes the file. A session that produced nothing (mode toggled on
// and straight back off) leaves no empty file behind.
func (n *Notes) Close() error {
	err := n.f.Close()
	if n.n == 0 {
		if rmErr := os.Remove(n.path); rmErr != nil && err == nil {
			err = rmErr
		}
	}
	return err
}
