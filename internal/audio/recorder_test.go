package audio

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestRecorderReadsUntilNaturalEOF(t *testing.T) {
	r, err := Start(context.Background(), "head -c 6400 /dev/zero", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 6400 {
		t.Fatalf("read %d bytes, want 6400", len(b))
	}
	if err := r.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestRecorderStopProducesEOFAndCleanExit(t *testing.T) {
	r, err := Start(context.Background(), "cat /dev/zero", nil)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3200)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	r.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := r.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("no EOF after Stop")
		}
	}
	if err := r.Wait(); err != nil {
		t.Fatalf("Wait after Stop: %v (a SIGTERM exit must be reported as clean)", err)
	}
}

func TestRecorderMissingCommand(t *testing.T) {
	if _, err := Start(context.Background(), "definitely-not-a-real-recorder-xyz -", nil); err == nil {
		t.Fatal("expected error for missing command")
	}
}
