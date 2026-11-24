#!/bin/bash
# SoundTouch Radio Bridge – Start per Doppelklick im Finder (macOS)
# Funktioniert im Projektordner (scripts/) oder neben strb + strb.yaml.

cd "$(dirname "$0")"
[ -f strb.yaml ] || { [ -f ../strb.yaml ] && cd ..; }

BIN=""
for c in ./strb ./bin/strb; do
  [ -x "$c" ] && { BIN="$c"; break; }
done

if [ -z "$BIN" ]; then
  echo
  echo "  strb nicht gefunden. Erst bauen:  go build -o bin/strb ./cmd/strb"
  echo
  read -n 1 -s -r -p "  Taste drücken zum Schließen …"
  exit 1
fi
if [ ! -f strb.yaml ]; then
  echo
  echo "  strb.yaml fehlt. Vorlage kopieren:  cp strb.example.yaml strb.yaml"
  echo
  read -n 1 -s -r -p "  Taste drücken zum Schließen …"
  exit 1
fi

printf '\033]0;SoundTouch Radio Bridge\007'
cat <<'TXT'

  ============================================
    SoundTouch Radio Bridge
  ============================================
    Tasten an der Box drücken – Radio läuft.
    Status:   http://127.0.0.1:8686/status
    Beenden:  Ctrl+C oder Fenster schließen

    Der Mac bleibt wach, solange strb läuft.
  ============================================

TXT

# caffeinate -i: kein Ruhezustand, solange strb läuft (Bildschirm darf aus)
caffeinate -i "$BIN" run "$@"

echo
echo "  strb wurde beendet."
read -n 1 -s -r -p "  Taste drücken zum Schließen …"
