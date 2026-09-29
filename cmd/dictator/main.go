// dictator is the client for dictatord. It is pure Go (no cgo, no shared
// libraries) so it starts in about a millisecond from a hotkey and works
// even when the daemon's native libraries are misconfigured.
//
//	dictator type|notes|idle        switch mode
//	dictator toggle type|notes      enter the mode, or leave it if already in it
//	dictator status [-json]         print the current mode
//	dictator watch [-format F]      stream state changes (F: plain|json|waybar)
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/varlogtim/dictator/internal/proto"
)

var version = "dev"

const usage = `usage: dictator [-socket PATH] [-notify] <command>

commands:
  type | notes | idle       switch to that mode
  toggle <type|notes>       enter the mode, or back to idle if already in it
  status [-json]            print the current mode (or the full state as JSON)
  watch [-format FMT]       stream state changes; FMT is plain, json or waybar
  version

flags:
  -socket PATH   daemon socket (default: $DICTATOR_SOCKET or $XDG_RUNTIME_DIR/dictator.sock)
  -notify        on failure, also raise a desktop notification (for hotkeys)
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("dictator", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	socketPath := fs.String("socket", proto.DefaultSocketPath(), "")
	notify := fs.Bool("notify", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, rest := fs.Arg(0), fs.Args()[1:]

	fail := func(err error) int {
		msg := err.Error()
		fmt.Fprintln(os.Stderr, "dictator:", msg)
		if *notify {
			_ = exec.Command("notify-send", "-a", "dictator", "-u", "critical", "dictator", msg).Run()
		}
		return 1
	}

	switch cmd {
	case proto.ModeType, proto.ModeNotes, proto.ModeIdle:
		st, err := request(*socketPath, proto.Request{Cmd: proto.CmdSet, Mode: cmd, Env: proto.ClientEnv()})
		if err != nil {
			return fail(err)
		}
		fmt.Println(st.Mode)
	case proto.CmdToggle:
		if len(rest) != 1 {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		st, err := request(*socketPath, proto.Request{Cmd: proto.CmdToggle, Mode: rest[0], Env: proto.ClientEnv()})
		if err != nil {
			return fail(err)
		}
		fmt.Println(st.Mode)
	case proto.CmdStatus:
		sub := flag.NewFlagSet("status", flag.ContinueOnError)
		asJSON := sub.Bool("json", false, "")
		if err := sub.Parse(rest); err != nil {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		st, err := request(*socketPath, proto.Request{Cmd: proto.CmdStatus})
		if err != nil {
			return fail(err)
		}
		if *asJSON {
			json.NewEncoder(os.Stdout).Encode(st)
		} else {
			fmt.Println(st.Mode)
		}
	case proto.CmdWatch:
		sub := flag.NewFlagSet("watch", flag.ContinueOnError)
		format := sub.String("format", "plain", "")
		if err := sub.Parse(rest); err != nil {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		render, ok := renderers[*format]
		if !ok {
			return fail(fmt.Errorf("unknown -format %q (plain|json|waybar)", *format))
		}
		return watch(*socketPath, render)
	case "version":
		fmt.Println("dictator", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "dictator: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	return 0
}

// request performs one command and returns the daemon's state. A refused
// connection or an ok=false reply is an error.
func request(socketPath string, req proto.Request) (*proto.State, error) {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dictatord not running? (%s: %v)", socketPath, errors.Unwrap(err))
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var resp proto.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("bad reply from daemon: %w", err)
	}
	if !resp.OK {
		return resp.State, errors.New(resp.Error)
	}
	return resp.State, nil
}

type renderer func(st proto.State) string

var renderers = map[string]renderer{
	"plain": func(st proto.State) string { return st.Mode },
	"json": func(st proto.State) string {
		b, _ := json.Marshal(st)
		return string(b)
	},
	"waybar": renderWaybar,
}

// renderWaybar emits waybar's custom-module JSON. "alt" selects the icon via
// format-icons and "class" drives CSS, so all presentation stays in the
// waybar config.
func renderWaybar(st proto.State) string {
	classes := []string{st.Mode}
	var tip []string
	tip = append(tip, "dictator: "+st.Mode)
	if t, err := time.Parse(time.RFC3339, st.Since); err == nil {
		tip = append(tip, "since "+t.Local().Format("15:04:05"))
	}
	if st.Detail != "" {
		tip = append(tip, st.Detail)
	}
	if len(st.Warnings) > 0 {
		classes = append(classes, "warning")
		tip = append(tip, st.Warnings...)
	}
	b, _ := json.Marshal(map[string]any{
		"text":    st.Mode,
		"alt":     st.Mode,
		"class":   classes,
		"tooltip": strings.Join(tip, "\n"),
	})
	return string(b)
}

// downState is what watch renders while the daemon is unreachable.
var downState = proto.State{Mode: "down", Detail: "dictatord not running"}

// watch streams states forever, reconnecting while the daemon is down. It
// only returns when stdout is gone (the consumer, e.g. waybar, exited).
func watch(socketPath string, render renderer) int {
	out := bufio.NewWriter(os.Stdout)
	emit := func(st proto.State) bool {
		if _, err := fmt.Fprintln(out, render(st)); err != nil {
			return false
		}
		return out.Flush() == nil
	}
	reportedDown := false
	for {
		err := watchOnce(socketPath, func(st proto.State) bool {
			reportedDown = false
			return emit(st)
		})
		if errors.Is(err, errStdoutClosed) {
			return 0
		}
		if !reportedDown {
			if !emit(downState) {
				return 0
			}
			reportedDown = true
		}
		time.Sleep(2 * time.Second)
	}
}

var errStdoutClosed = errors.New("stdout closed")

func watchOnce(socketPath string, onState func(proto.State) bool) error {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(proto.Request{Cmd: proto.CmdWatch}); err != nil {
		return err
	}
	dec := json.NewDecoder(bufio.NewReader(conn))
	for {
		var resp proto.Response
		if err := dec.Decode(&resp); err != nil {
			return err
		}
		if resp.State == nil {
			continue
		}
		if !onState(*resp.State) {
			return errStdoutClosed
		}
	}
}
