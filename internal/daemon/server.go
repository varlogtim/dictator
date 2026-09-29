package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"

	"github.com/varlogtim/dictator/internal/proto"
)

// Server exposes a Controller over a Unix socket using the newline-delimited
// JSON protocol in package proto.
type Server struct {
	c    *Controller
	ln   net.Listener
	path string
	log  *slog.Logger
}

// Listen binds the socket. A stale socket file left by a crashed daemon is
// removed; a live one (another daemon answering) is an error.
func Listen(path string, c *Controller, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if conn, err := net.Dial("unix", path); err == nil {
		conn.Close()
		return nil, fmt.Errorf("another dictatord is already listening on %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return &Server{c: c, ln: ln, path: path, log: log}, nil
}

// Path is the socket path.
func (s *Server) Path() string { return s.path }

// Serve accepts connections until Close is called.
func (s *Server) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(conn)
	}
}

// Close stops accepting and removes the socket file.
func (s *Server) Close() error {
	err := s.ln.Close()
	_ = os.Remove(s.path)
	return err
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	enc := json.NewEncoder(conn)
	reply := func(resp proto.Response) bool {
		return enc.Encode(resp) == nil
	}

	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) == 0 {
			if err != nil {
				return
			}
			continue
		}
		var req proto.Request
		if jerr := json.Unmarshal(line, &req); jerr != nil {
			if !reply(proto.Response{Error: "bad request: " + jerr.Error()}) {
				return
			}
			continue
		}

		switch req.Cmd {
		case proto.CmdStatus:
			st := s.c.Status()
			if !reply(proto.Response{OK: true, State: &st}) {
				return
			}
		case proto.CmdSet, proto.CmdToggle:
			var st proto.State
			var cerr error
			if req.Cmd == proto.CmdSet {
				st, cerr = s.c.Set(req.Mode, req.Env)
			} else {
				st, cerr = s.c.Toggle(req.Mode, req.Env)
			}
			resp := proto.Response{OK: cerr == nil, State: &st}
			if cerr != nil {
				resp.Error = cerr.Error()
			}
			if !reply(resp) {
				return
			}
		case proto.CmdWatch:
			s.watch(conn, r, reply)
			return
		default:
			if !reply(proto.Response{Error: fmt.Sprintf("unknown command %q", req.Cmd)}) {
				return
			}
		}
		if err != nil {
			return // EOF after the last line
		}
	}
}

// watch streams state changes until the client hangs up. The client is not
// expected to send anything more; its reader is used only to notice EOF.
func (s *Server) watch(conn net.Conn, r io.Reader, reply func(proto.Response) bool) {
	current, updates, cancel := s.c.Subscribe()
	defer cancel()
	if !reply(proto.Response{OK: true, State: &current}) {
		return
	}
	closed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, r)
		close(closed)
	}()
	for {
		select {
		case st := <-updates:
			if !reply(proto.Response{OK: true, State: &st}) {
				return
			}
		case <-closed:
			return
		}
	}
}
