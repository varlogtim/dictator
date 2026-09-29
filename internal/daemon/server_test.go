package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/varlogtim/dictator/internal/proto"
)

func startTestServer(t *testing.T) (*Server, *Controller) {
	t.Helper()
	c, _ := newReadyController(t, realtimeRecorder(t))
	// Keep the path short: AF_UNIX paths are limited to ~108 bytes.
	path := filepath.Join(t.TempDir(), "d.sock")
	srv, err := Listen(path, c, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	return srv, c
}

func roundTrip(t *testing.T, path string, req proto.Request) proto.Response {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatal(err)
	}
	var resp proto.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestServerStatusSetToggle(t *testing.T) {
	srv, _ := startTestServer(t)

	resp := roundTrip(t, srv.Path(), proto.Request{Cmd: proto.CmdStatus})
	if !resp.OK || resp.State == nil || resp.State.Mode != proto.ModeIdle {
		t.Fatalf("status: %+v", resp)
	}
	resp = roundTrip(t, srv.Path(), proto.Request{Cmd: proto.CmdToggle, Mode: proto.ModeNotes})
	if !resp.OK || resp.State.Mode != proto.ModeNotes {
		t.Fatalf("toggle notes: %+v", resp)
	}
	resp = roundTrip(t, srv.Path(), proto.Request{Cmd: proto.CmdSet, Mode: "bogus"})
	if resp.OK || resp.Error == "" || resp.State.Mode != proto.ModeNotes {
		t.Fatalf("set bogus should fail and report the unchanged state: %+v", resp)
	}
	resp = roundTrip(t, srv.Path(), proto.Request{Cmd: proto.CmdSet, Mode: proto.ModeIdle})
	if !resp.OK || resp.State.Mode != proto.ModeIdle {
		t.Fatalf("set idle: %+v", resp)
	}
	resp = roundTrip(t, srv.Path(), proto.Request{Cmd: "dance"})
	if resp.OK || resp.Error == "" {
		t.Fatalf("unknown command: %+v", resp)
	}
}

func TestServerWatchStreamsTransitions(t *testing.T) {
	srv, c := startTestServer(t)

	conn, err := net.Dial("unix", srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(proto.Request{Cmd: proto.CmdWatch}); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bufio.NewReader(conn))
	next := func() proto.State {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var resp proto.Response
		if err := dec.Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if !resp.OK || resp.State == nil {
			t.Fatalf("bad watch frame: %+v", resp)
		}
		return *resp.State
	}
	if st := next(); st.Mode != proto.ModeIdle {
		t.Fatalf("initial frame %q", st.Mode)
	}
	if _, err := c.Set(proto.ModeNotes, nil); err != nil {
		t.Fatal(err)
	}
	if st := next(); st.Mode != proto.ModeNotes {
		t.Fatalf("frame %q, want notes", st.Mode)
	}
	if _, err := c.Set(proto.ModeIdle, nil); err != nil {
		t.Fatal(err)
	}
	if st := next(); st.Mode != proto.ModeIdle {
		t.Fatalf("frame %q, want idle", st.Mode)
	}
}

func TestListenRefusesLiveSocketAndReplacesStaleOne(t *testing.T) {
	srv, c := startTestServer(t)
	if _, err := Listen(srv.Path(), c, testLogger(t)); err == nil {
		t.Fatal("second Listen on a live socket must fail")
	}
	srv.ln.Close() // simulate a crash: file may linger, nobody answers
	if _, err := net.Dial("unix", srv.Path()); err == nil {
		t.Fatal("precondition: socket should be dead")
	}
	srv2, err := Listen(srv.Path(), c, testLogger(t))
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	srv2.Close()
}
