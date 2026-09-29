#!/usr/bin/env bash
# End-to-end smoke test for dictatord + dictator with NO microphone and NO
# compositor: the recorder is `cat` of a raw PCM file and wtype is a stub
# that logs what it would have typed. Uses the real model, so it also
# verifies the native libraries load.
#
# Usage: scripts/smoke.sh [path/to/audio.raw]
#   DICTATOR_MODEL_DIR / DICTATOR_VAD_MODEL  override model locations
#   DICTATORD / DICTATOR                     override binaries (default: build/)
#
# The raw file must be 16 kHz mono s16le. Without an argument the script
# builds one from the model's bundled test_wavs/0.wav (twice, with silence).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
DICTATORD=${DICTATORD:-$here/build/dictatord}
DICTATOR=${DICTATOR:-$here/build/dictator}
data=${XDG_DATA_HOME:-$HOME/.local/share}/dictator/models
MODEL_DIR=${DICTATOR_MODEL_DIR:-$data/sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8}
VAD_MODEL=${DICTATOR_VAD_MODEL:-$data/silero_vad.onnx}

tmp=$(mktemp -d /tmp/dictator-smoke.XXXXXX)
cleanup() {
  [[ -n "${dpid:-}" ]] && kill "$dpid" 2>/dev/null && wait "$dpid" 2>/dev/null
  [[ -n "${wpid:-}" ]] && kill "$wpid" 2>/dev/null
  rm -rf "$tmp"
}
trap cleanup EXIT

audio=${1:-}
if [[ -z "$audio" ]]; then
  audio=$tmp/audio.raw
  python3 - "$MODEL_DIR/test_wavs/0.wav" "$audio" <<'EOF'
import sys, wave
w = wave.open(sys.argv[1]); assert (w.getframerate(), w.getnchannels(), w.getsampwidth()) == (16000, 1, 2), "test wav must be 16k mono s16"
pcm = w.readframes(w.getnframes()); sil = b"\0\0" * 16000
open(sys.argv[2], "wb").write(sil + pcm + sil + pcm + sil)
EOF
fi

# Stub wtype: records its text argument (after "--") one per line.
mkdir -p "$tmp/bin"
cat >"$tmp/bin/wtype" <<EOF
#!/bin/sh
printf '%s\n' "\$2" >> "$tmp/typed.log"
EOF
chmod +x "$tmp/bin/wtype"
export PATH="$tmp/bin:$PATH"

# Stub recorder: a real microphone yields nothing for the first instant; cat
# would dump the whole file before an immediate toggle-off could stop it.
cat >"$tmp/bin/recorder" <<EOF
#!/bin/sh
sleep 0.3
exec cat "$audio"
EOF
chmod +x "$tmp/bin/recorder"

sock=$tmp/sock
notes=$tmp/notes
echo "==> starting dictatord (model: $MODEL_DIR)"
"$DICTATORD" -socket "$sock" -model-dir "$MODEL_DIR" -vad-model "$VAD_MODEL" \
  -notes-dir "$notes" -record-cmd "$tmp/bin/recorder" -log-level debug >"$tmp/daemon.log" 2>&1 &
dpid=$!

c() { "$DICTATOR" -socket "$sock" "$@"; }
wait_mode() { # wait_mode MODE [seconds]
  local want=$1 deadline=$(( $(date +%s) + ${2:-30} ))
  while (( $(date +%s) < deadline )); do
    [[ "$(c status 2>/dev/null || true)" == "$want" ]] && return 0
    sleep 0.2
  done
  echo "timed out waiting for mode $want; daemon log:"; cat "$tmp/daemon.log"; return 1
}

echo "==> watcher (waybar format) running in background"
c watch -format waybar >"$tmp/watch.log" 2>&1 &
wpid=$!

wait_mode idle 60
echo "==> idle after model load"

echo "==> notes mode: recorder plays the file, then ends -> back to idle"
c toggle notes
wait_mode idle 30
notes_file=$(ls "$notes"/*.md)
echo "--- $notes_file"; cat "$notes_file"
lines=$(wc -l <"$notes_file")
(( lines >= 2 )) || { echo "FAIL: expected >= 2 utterances in notes, got $lines"; exit 1; }

echo "==> type mode with stub wtype"
WAYLAND_DISPLAY=wayland-smoke c toggle type
wait_mode idle 30
echo "--- typed.log"; cat "$tmp/typed.log"
typed=$(wc -l <"$tmp/typed.log")
(( typed == lines )) || { echo "FAIL: typed $typed utterances, notes had $lines"; exit 1; }

echo "==> status -json"
c status -json

echo "==> toggle on then immediately off (nothing spoken -> no empty notes file)"
c toggle notes >/dev/null; c toggle notes >/dev/null
sleep 1
n=$(ls "$notes"/*.md | wc -l)
(( n == 1 )) || { echo "FAIL: expected exactly 1 notes file, found $n"; ls -la "$notes"; exit 1; }

echo "==> SIGTERM daemon; socket must be removed"
kill "$dpid"; wait "$dpid" || true; unset dpid
[[ ! -e "$sock" ]] || { echo "FAIL: socket left behind"; exit 1; }
sleep 2.5
echo "--- watch.log (last frames)"; tail -n 6 "$tmp/watch.log"
grep -q '"alt":"down"' "$tmp/watch.log" || { echo "FAIL: watcher never reported daemon down"; exit 1; }

echo "PASS"
