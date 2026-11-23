# soundtouch-radio-bridge (`strb`)

Erweckt die **Preset-Tasten 1–6** von Bose-SoundTouch-Lautsprechern wieder zum Leben, seit Bose die SoundTouch-Cloud am 6. Mai 2026 abgeschaltet hat.

`strb` läuft im Heimnetz (macOS, Windows, Linux). Es hört über den lokalen WebSocket der Box auf Tastendrücke und spielt den dafür festgelegten Internet-Radiosender über UPnP ab. Dafür braucht es weder Cloud noch Home Assistant, und an der Firmware wird nichts verändert.

> *Revives the preset buttons of Bose SoundTouch speakers after the 2026 cloud shutdown – a small, dependency-free Go service for your home network.*

**Status:** M1 fertig. Läuft am Referenzgerät, einem SoundTouch-10-Stereo-Paar mit Firmware 27.0.6. Siehe die [Spezifikation](docs/SPEC.md) und die [Befunde am Gerät](docs/M0-FINDINGS.md).

## Schnellstart

Voraussetzung ist Go 1.23 oder neuer. Fertige Programmdateien gibt es noch nicht.

```bash
go build -o strb ./cmd/strb
./strb discover                      # Boxen finden, deviceID notieren
cp strb.example.yaml strb.yaml       # Sender und Box eintragen
./strb run                           # läuft, bis Ctrl+C
```

Danach spielen die Preset-Tasten 1–6 der Box die Sender aus `strb.yaml`. Änderungen an der Datei übernimmt `strb` ohne Neustart. Die bisherigen Presets der Box werden vorher in `backups/` gesichert, und `strb restore <datei>` spielt sie zurück.

| Befehl | Zweck |
|---|---|
| `strb run` | Normalbetrieb |
| `strb discover` | Boxen im Netz anzeigen, inklusive Stereo-Paar-Rolle |
| `strb presets [box]` | gespeicherte Presets anzeigen |
| `strb sync` | Presets einmalig umschreiben |
| `strb play <box> <taste\|sender>` | Sender testweise abspielen |
| `strb restore <datei> [box]` | gesicherte Presets zurückspielen |
| `strb listen <box>` | rohe Meldungen der Box (Diagnose) |

**Wichtig:** `strb` muss laufen, damit die Tasten funktionieren. Wenn der Rechner schläft, passiert beim Tastendruck nichts.

## Wie es funktioniert

1. Die Presets der Box zeigen nach dem Umschreiben auf `strb`. Die alten Presets werden vorher gesichert.
2. Pro Box hält `strb` eine WebSocket-Verbindung (`ws://box:8080`, Protokoll `gabbo`) offen und erkennt darüber den Tastendruck.
3. Über UPnP-AVTransport (`:8091`) startet `strb` den Sender. Ein eingebauter Proxy übernimmt dabei HTTPS und Redirects.

## Danke

Aufbauend auf den Erkenntnissen von [sandervg/homeassistant-bose-soundtouch-bridge](https://github.com/sandervg/homeassistant-bose-soundtouch-bridge), [Air-Drift/Sound-Touch](https://github.com/Air-Drift/Sound-Touch), [gesellix/Bose-SoundTouch](https://github.com/gesellix/Bose-SoundTouch) und [deborahgu/soundcork](https://github.com/deborahgu/soundcork).

Bose und SoundTouch sind Marken der Bose Corporation. Dieses Projekt steht in keiner Verbindung zu Bose.

## Lizenz

MIT
