# dictator

Local, always-warm speech-to-text for a sway/Wayland desktop. A hotkey puts
the daemon in one of three states:

| state   | what happens                                                                 |
|---------|------------------------------------------------------------------------------|
| `idle`  | model loaded, microphone released                                            |
| `type`  | each phrase you say is typed into whatever window has focus (via `wtype`)    |
| `notes` | each phrase is appended, one per line, to `~/notes/dictation/<start-time>.md` |

Recognition is NVIDIA **Parakeet TDT 0.6B v2** (cased, punctuated English)
running on the CPU through [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx)
/ ONNX Runtime; Silero VAD cuts speech into phrases. Nothing leaves the
machine. On a Ryzen 5 3600 the model loads in ~2.5 s, sits at ~800 MB RSS,
decodes a 4 s phrase in ~300 ms, and the VAD costs under 1 % of a core while
listening. When idle it costs nothing but memory.

## How it works

```
 hotkey ──▶ dictator toggle type ──┐
 hotkey ──▶ dictator toggle notes ─┼─▶ unix socket ─▶ ┌──────────── dictatord ─────────────┐
                                   │                  │ controller: owns the mode           │
 waybar ◀── dictator watch ◀───────┘   (fan-out)      │   session: pw-record ─▶ VAD ─▶      │
                                                      │            decode ─▶ sink           │
                                                      │ model stays loaded across sessions  │
                                                      └─────────────────────────────────────┘
                                                        sink "type"  = wtype per phrase
                                                        sink "notes" = one file per session
```

- **Two binaries.** `dictatord` (cgo, owns the model) and `dictator` (pure Go,
  static, ~1 ms start-up — the thing hotkeys and waybar call).
- **Capture only while listening.** Entering a mode spawns
  `pw-record --rate 16000 --channels 1 --format s16 -`; leaving sends it
  SIGTERM. EOF then flows down the pipeline, so the phrase you were finishing
  is still decoded and delivered before the sink closes. While idle the mic
  is released (waybar's `privacy` dot goes out).
- **Ordered delivery.** Phrases are decoded and delivered sequentially per
  session; decodes are serialized across sessions too, so switching modes
  mid-sentence sends that sentence to the mode you were in.
- **Push, not poll.** `dictator watch` holds a connection and prints a line
  per state change; waybar runs it as a continuous custom module.
- **Environment plumbing.** `wtype` needs `WAYLAND_DISPLAY`, but a user
  service may start before the compositor. The client sends its own
  `WAYLAND_DISPLAY`/`XDG_RUNTIME_DIR` with each request and the daemon uses
  them for that session's `wtype` (falling back to `$XDG_RUNTIME_DIR/wayland-*`).

## Install

Requirements: Go ≥ 1.24, gcc (cgo), PipeWire (`pw-record`), and for `type`
mode `wtype` (`sudo apt install wtype`). The daemon warns at start-up, in
`dictator status`, and in the waybar tooltip if either tool is missing;
installing `wtype` later needs no restart.

```sh
make models     # ~630 MB: Silero VAD + Parakeet into ~/.local/share/dictator/models
make install    # builds; copies binaries to ~/.local/bin, libs to ~/.local/lib/dictator,
                # the unit to ~/.config/systemd/user, an env file to ~/.config/dictator
make enable     # systemctl --user enable --now dictator
dictator status # -> idle (after ~3 s; "starting" while the model loads)
```

Then add the two hotkeys from `contrib/sway.conf` and the module from
`contrib/waybar.jsonc` + `contrib/waybar.css`. `make reinstall` rebuilds and
restarts the service after a code change.

Native libraries: the Go binding links two prebuilt shared objects
(`libsherpa-onnx-c-api.so`, `libonnxruntime.so`, ~32 MB, CPU-only). The
binary carries an rpath to `$ORIGIN/../lib/dictator` (where `make install`
copies them) and a fallback rpath into the Go module cache.

## Use

```sh
dictator type | notes | idle     # switch mode
dictator toggle type             # enter type mode, or leave it if already there
dictator status [-json]          # current mode; -json shows since/detail/warnings
dictator watch [-format waybar]  # stream state changes (plain|json|waybar)
```

`-notify` (before the command) raises a desktop notification if the request
fails — daemon down, model still loading, `wtype` missing — which is what
you want from a hotkey. Otherwise the waybar module is the only feedback.

Notes mode writes `~/notes/dictation/2026-09-29_09-41-05.md` (start time of
the session), one phrase per line, visible to `tail -f` as you speak. A
session in which nothing was said leaves no file behind.

## Configuration

Every setting is a `dictatord` flag with a `DICTATOR_*` environment variable
of the same name; the service reads `~/.config/dictator/dictator.env`
(annotated example in `contrib/dictator.env.example`). The ones worth
knowing:

| variable                   | default                           | purpose                                      |
|----------------------------|-----------------------------------|----------------------------------------------|
| `DICTATOR_NOTES_DIR`       | `~/notes/dictation`               | where notes sessions are written             |
| `DICTATOR_MODEL_DIR`       | `…/models/sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8` | any sherpa-onnx NeMo transducer or CTC model dir |
| `DICTATOR_THREADS`         | `4`                               | recognizer threads                           |
| `DICTATOR_RECORD_CMD`      | `pw-record --rate 16000 --channels 1 --format s16 -` | any producer of 16 kHz mono s16le on stdout |
| `DICTATOR_TYPE_SUFFIX`     | `" "`                             | typed after each phrase                      |
| `DICTATOR_TYPE_DELAY_MS`   | `0`                               | per-keystroke delay if an app drops input    |
| `DICTATOR_VAD_MIN_SILENCE` | `0.4`                             | seconds of silence that end a phrase         |
| `DICTATOR_LOG_LEVEL`       | `info`                            | `debug` logs every phrase                    |

Logs: `journalctl --user -u dictator -f`.

## Protocol

Newline-delimited JSON over `$XDG_RUNTIME_DIR/dictator.sock`:

```sh
printf '{"cmd":"status"}\n' | nc -U "$XDG_RUNTIME_DIR/dictator.sock"
printf '{"cmd":"toggle","mode":"notes"}\n' | nc -U "$XDG_RUNTIME_DIR/dictator.sock"
printf '{"cmd":"watch"}\n' | nc -U "$XDG_RUNTIME_DIR/dictator.sock"   # streams
```

Commands: `status`, `set {mode}`, `toggle {mode}`, `watch`. Replies:
`{"ok":true,"state":{"mode":…,"since":…,"detail":…,"modes":[…],"warnings":[…]}}`
or `{"ok":false,"error":…,"state":…}`. See `internal/proto`.

## Design notes

- *Why a daemon at all:* the model takes seconds to load and ~800 MB; keeping
  it warm is the whole point. The socket comes up before the model loads so
  clients see `starting` rather than a refused connection.
- *Why not REST/HTTP:* there is no remote client and no resource model; three
  verbs and a stream fit a line protocol, are debuggable with `nc`, and match
  how `swaymsg`/`swaync-client` work.
- *Why exec `pw-record` and `wtype` rather than bind PipeWire/Wayland:* no
  extra cgo, both stages are swappable from config, and SIGTERM→EOF gives a
  natural drain-on-stop. `wtype` reads all input before typing, so one
  process per phrase is the right unit anyway.
- *Why phrase-at-a-time rather than word-by-word streaming:* keystrokes
  cannot be un-typed and streaming models revise their output. Committing at
  a VAD pause is what every tool that types into arbitrary apps does; with
  this model the text lands ~0.5 s after you stop talking.
- *Adding a mode:* implement `sink.Sink`, register it in
  `daemon.New` (`c.register(name, factory)`), done — the client, protocol and
  indicator are mode-agnostic.

## Development

```sh
make test        # unit tests (fake engine/recorder; -race)
make test-model  # internal/asr against the real models
make smoke       # end-to-end: real model, stub recorder + stub wtype, no mic needed
```

## Gotchas

- `type` mode types wherever the caret is. In nvim be in insert mode.
- Hold-to-talk on a `$mod` chord would be a trap: wlroots merges modifier
  state across keyboards, so synthetic `a` while `Super` is held becomes
  `Super+a`. The toggle bindings avoid this.
- Two notes sessions started within the same second share a file
  (`O_APPEND`); harmless.
