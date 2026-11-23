# SoundTouch Radio Bridge – Spezifikation

| | |
|---|---|
| Status | **v0.3** – M0 abgeschlossen, Ansatz am Gerät bestätigt ([Befunde](M0-FINDINGS.md)) |
| Datum | 2026-11-23 |
| Name | `soundtouch-radio-bridge`, Befehl `strb` |
| Sprache / Stack | Go |
| Repo | öffentlich auf GitHub |

---

## 1. Ziel

Ein kleines Programm, das im Heimnetz läuft und die **Preset-Tasten 1–6** der Bose-SoundTouch-Geräte wieder zum Leben erweckt:
Taste drücken → festgelegter Internet-Radiosender spielt. Ohne Bose-Cloud, ohne Home Assistant, ohne Eingriff in die Firmware.

### Muss (Version 1)
- Tastendruck an der Box (Preset 1–6) zuverlässig erkennen.
- Den dafür konfigurierten Radio-Stream auf genau dieser Box abspielen, inklusive Sendername im Display.
- Mehrere Boxen gleichzeitig.
- Sender in einer Konfigurationsdatei festlegen, keine Oberfläche.
- Sender lassen sich **jederzeit ändern**: Datei bearbeiten und speichern, `strb` übernimmt die Änderung ohne Neustart (siehe 4).
- Läuft auf macOS, später auch auf Windows (und nebenbei auf Linux oder einem Raspberry Pi).
- **Zuverlässigkeit ist das wichtigste Ziel.** Genau daran scheitern die bestehenden Lösungen (siehe Abschnitt 7).

### Nicht-Ziele (Version 1)
- Keine Oberfläche, keine App.
- Keine Cloud-Emulation (Marge/BMX) und kein Umbiegen der Boxen-Konfiguration (Port 17000 oder USB-Stick).
- Kein Spotify, Bluetooth, AUX und keine Lautstärke-Automatik. Das alles funktioniert weiter wie bisher.
- Keine Gruppen und kein Multiroom-Zonen-Management.
- Keine Home-Assistant-Integration (vielleicht später).

---

## 2. Ausgangslage

### 2.1 Bose
- Die SoundTouch-Cloud ist seit **6. Mai 2026** abgeschaltet.
- Bose hat die **lokale Web-API dokumentiert**, aber keinen Code veröffentlicht.
- Die Presets der Boxen zeigen auf `TUNEIN`-Sender. Diese werden über die Bose-Cloud aufgelöst und laufen deshalb nicht mehr.

### 2.2 Referenz-Geräte (Bestand am 2026-11-23)

| Modell | Firmware | Im Umfang |
|---|---|---|
| 2× SoundTouch 10 als Stereo-Paar (Bose Stereo Link, „Links“ = Master) | 27.0.6.46330 | **ja**, Referenzgerät für M0–M2 |
| Cinemate (SoundTouch-Modul, variant „lisa“) | 27.0.6.46330 | vorerst nicht (Modell wird aber nicht ausgeschlossen) |

> Geräte-IDs, IPs und Seriennummern stehen bewusst **nicht** im Repo. Sie gehören in die lokale `strb.yaml` (in `.gitignore`).

Auf beiden Geräten geprüft:
- Offen sind die Ports **8090** (Web-API), **8080** (WebSocket, Protokoll `gabbo`), **8091** (UPnP mit AVTransport unter `/AVTransport/Control`) und 17000 (Service-Konsole, wird nicht genutzt).
- Der WebSocket meldet sich mit `<SoundTouchSdkInfo serverVersion="4" .../>`.
- Die `.local`-Hostnamen der Boxen (`Bose-SM2-….local`) ließen sich mit curl nicht zuverlässig auflösen. **Intern arbeiten wir deshalb mit IP-Adressen** und erkennen Geräte an ihrer `deviceID`.

### 2.3 Start-Belegung der Presets

Übernommen aus der bisherigen Belegung über Home Assistant und Music Assistant. Taste 3 hat einen Ersatz bekommen, weil der Sender bei TuneIn nicht mehr streamt. Die Belegung lässt sich jederzeit in der Konfiguration ändern.

| # | Sender | Stream-URL (geprüft am 2026-11-23) |
|---|---|---|
| 1 | SWR3 | `https://liveradio.swr.de/sw282p3/swr3/play.mp3` |
| 2 | Jazz Cafe | `http://radio.wanderingsheep.tv:8000/jazzcafe` |
| 3 | 101 Smooth Jazz | `http://jking.cdnstream1.com/b22139_128mp3` |
| 4 | SomaFM Groove Salad | `https://ice2.somafm.com/groovesalad-128-mp3` |
| 5 | Midnight Cafe Radio | `http://vh11367x.out.airtime.pro:8000/vh11367x_a` |
| 6 | Chilltrax | `https://streamssl.chilltrax.com/stream` |

Alle sechs liefern `audio/mpeg`. Drei davon laufen über HTTPS, das die Box selbst nicht abspielen kann. Deshalb braucht es den Proxy aus Abschnitt 5.4.

---

## 3. Funktionsweise (Überblick)

Ansatz „Mithören und Abspielen“. So arbeiten auch die bestehenden Projekte sandervg/bose-soundtouch-bridge und Air-Drift/Sound-Touch.

```
           (1) Presets einmalig umschreiben: POST :8090/storePreset  (Quelle UPNP, Ort = unser Proxy)
  ┌──────────────────────────────────────────────────────────────────────────┐
  │                                                                          ▼
┌─┴────────────────────┐   (2) ws://box:8080 (gabbo)                ┌──────────────────┐
│     strb (Go)        │◄────── nowSelectionUpdated preset id=N ───│  SoundTouch-Box  │
│                      │                                           │                  │
│  Listener  Player    │── (3) UPnP :8091 Stop → SetAVTransportURI → Play ─►│                  │
│  Proxy     Sync      │                                           │                  │
│                      │◄── (4) GET http://strb:8686/stream/<id>/N ─│                  │
└─────────┬────────────┘                                           └──────────────────┘
          │ (5) holt den echten Stream (http/https, Redirects)
          ▼
   Radiosender im Internet
```

1. **Preset-Sync:** Beim Start schreibt `strb` jedes Preset auf jeder Box so um, dass es auf einen lokalen Proxy-Link zeigt (Quelle `UPNP`).
   - Der alte TuneIn-Inhalt wird vorher gesichert.
   - Nach dem Umschreiben zeigt die Box beim Drücken den Sendernamen und meldet den Tastendruck.
2. **Tastendruck erkennen:** Ein dauerhafter WebSocket pro Box. Die Box schickt `nowSelectionUpdated` mit `<preset id="N">`.
3. **Abspielen:** Über UPnP-AVTransport bekommt die Box den Proxy-Link plus DIDL-Lite-Metadaten (Sendername, Logo) und startet die Wiedergabe.
4. **Die Box holt den Stream** beim eingebauten Proxy, und zwar immer über einfaches `http://`.
5. **Der Proxy holt den echten Sender** (auch `https`, folgt Redirects) und reicht die Bytes 1:1 durch.

> **In M0 geklärt:** Die Box spielt ein `UPNP`-Preset beim Tastendruck **nicht** selbst ab. Sie meldet nur den Druck und danach Fehler 1036 (`UpnpRcvdContentItemInWrongState`). Schritt 3 ist also Pflicht. Das Umschreiben der Presets (Schritt 1) bleibt trotzdem sinnvoll: Die Box zeigt den Namen an, die Meldung kommt sauber mit unserem Link, und die Box versucht nicht mehr, TuneIn zu erreichen.

---

## 4. Konfiguration

Eine Datei `strb.yaml`. Gesucht wird sie im aktuellen Verzeichnis, dann im Benutzer-Konfigurationsordner oder über `--config`.
Im Repo liegt nur `strb.example.yaml`. Die echte `strb.yaml` ist in `.gitignore`.

```yaml
# Adresse, unter der die Boxen strb erreichen. Leer = automatisch ermitteln
# (Netzwerkschnittstelle, über die die Boxen erreichbar sind).
listen: ":8686"
public_url: ""            # z.B. "http://192.168.1.10:8686"

stations:
  swr3:          { name: "SWR3",                url: "https://liveradio.swr.de/sw282p3/swr3/play.mp3", logo: "" }
  jazzcafe:      { name: "Jazz Cafe",           url: "http://radio.wanderingsheep.tv:8000/jazzcafe" }
  smoothjazz101: { name: "101 Smooth Jazz",     url: "http://jking.cdnstream1.com/b22139_128mp3" }
  groovesalad:   { name: "SomaFM Groove Salad", url: "https://ice2.somafm.com/groovesalad-128-mp3" }
  midnightcafe:  { name: "Midnight Cafe Radio", url: "http://vh11367x.out.airtime.pro:8000/vh11367x_a" }
  chilltrax:     { name: "Chilltrax",           url: "https://streamssl.chilltrax.com/stream" }

# Standard-Belegung für alle Boxen (Taste 1–6)
presets: [swr3, jazzcafe, smoothjazz101, groovesalad, midnightcafe, chilltrax]

# Boxen: leer = automatisch finden. Optional fest per IP; abweichende Presets pro Box möglich.
speakers:
  - id: AABBCCDDEEFF          # deviceID (maßgeblich), siehe `strb discover`
    host: 192.168.1.50        # Startwert, wird bei Änderung per Discovery nachgeführt
    # presets: [swr3, ...]    # optionale Abweichung
# Nur hier gelistete Boxen werden angefasst. Ist die Liste leer, werden alle gefundenen Boxen verwendet.
```

Regeln:
- Ungültige Konfiguration lässt den Start mit einer klaren Fehlermeldung abbrechen (Zeile, Feld).
- Fehlt eine Box, ist das **kein** Fehler. `strb` wartet auf sie.
- **Änderungen im laufenden Betrieb:** Die Datei wird überwacht, Prüfintervall ca. 2 s.
  - Ist die neue Fassung gültig, wird sie übernommen, und die Presets der Boxen werden neu abgeglichen. Das Log zeigt eine kurze Zusammenfassung, z.B. „Preset 2: Lounge FM → Radio Paradise“.
  - Ist sie ungültig, läuft die alte Konfiguration weiter, und das Log zeigt eine Fehlermeldung.
  - Eine Änderung an `listen` erfordert einen Neustart, das wird geloggt.

---

## 5. Komponenten

Paketstruktur (Vorschlag):

```
cmd/strb/            Einstiegspunkt, CLI
internal/config/     Laden und Prüfen der YAML-Datei
internal/discovery/  SSDP-Suche (+ optional mDNS _soundtouch._tcp)
internal/speaker/    Client für die Web-API :8090 (info, presets, storePreset, now_playing)
internal/events/     WebSocket-Listener :8080 (gabbo), Parser für die Meldungen
internal/upnp/       AVTransport-SOAP :8091 (Stop, SetAVTransportURI, Play), DIDL-Lite
internal/proxy/      HTTP-Stream-Proxy
internal/bridge/     Steuerlogik: pro Box ein Supervisor, der alles zusammenführt
```

Abhängigkeiten so wenig wie möglich: ein WebSocket-Paket (`github.com/coder/websocket`), ein YAML-Paket. Alles andere (SSDP, SOAP, XML) kommt aus der Go-Standardbibliothek.

### 5.1 Discovery
- SSDP `M-SEARCH`, `ST: urn:schemas-upnp-org:device:MediaRenderer:1`, auf allen passenden IPv4-Schnittstellen, zweimal gesendet.
- Ein Kandidat gilt als SoundTouch, wenn `GET :8090/info` eine `deviceID` liefert.
- Läuft beim Start und danach alle 5 Minuten im Hintergrund, außerdem sofort, wenn eine Box nicht erreichbar ist.
  - Hat eine bekannte `deviceID` eine neue IP, wird automatisch umgezogen: WebSocket neu verbinden, UPnP-Cache leeren.

### 5.2 Event-Listener (je Box)
- `ws://<ip>:8080`, Subprotokoll `gabbo`.
- Auswertung: `<nowSelectionUpdated>` → erstes `<preset id="N">` mit N in 1–6 → Ereignis „Preset N“.
  - `preset id="0"` kommt bei der Power-Taste und wird ignoriert.
  - Das Ereignis kommt unabhängig davon, was im Preset steht (TuneIn oder UPNP). Damit funktioniert die Taste auch, wenn das Umschreiben noch nicht geklappt hat.
- `errorUpdate` und `groupUpdated` werden ausgewertet und geloggt (siehe 5.6).
- Alle anderen Meldungen werden ignoriert, aber im Debug-Log protokolliert.
- Robust parsen: Ungültiges XML wird geloggt und verworfen, es bringt das Programm nie zum Absturz.
- **Entprellen:** Dasselbe Preset innerhalb von 1,5 s gilt als **ein** Druck.
- Lebenszeichen und Neuverbindung: siehe Abschnitt 7.

### 5.3 Player (UPnP)
- Den Steuerlink holen wir aus `:8091/XD/BO5EBO5E-F00D-F00D-FEED-<deviceID>.xml`. Rückfalllösung: `/AVTransport/Control`. Pro Box zwischenspeichern, bei einem Fehler neu holen.
- Ablauf: `Stop` (Fehler ignorieren), dann `SetAVTransportURI` (Proxy-Link + DIDL-Lite mit `dc:title`, `upnp:class=object.item.audioItem.audioBroadcast`, optional `upnp:albumArtURI`, `res protocolInfo="http-get:*:audio/mpeg:*"`), dann `Play` mit Speed 1.
- Timeout 8 s pro Aufruf. Bei einem Fehler **ein** erneuter Versuch nach 1 s, danach Fehler-Log.

### 5.4 Stream-Proxy
- Endpunkt: `GET /stream/<deviceID>/<slot>` (auch `HEAD`). Ein zweiter Endpunkt `GET /station/<key>` dient zum Testen.
- Holt den Sender per `http` oder `https`, folgt Redirects (höchstens 5), setzt einen neutralen User-Agent.
- Leitet **keinen `Range`-Header** an den Sender weiter. SomaFM antwortet darauf zum Beispiel mit `403` (beobachtet am 2026-11-23).
- Reicht `Content-Type` und ICY-Header (`icy-name`, `icy-br`, `icy-genre`, `icy-metaint`) durch. `Icy-MetaData` wird nur angefordert, wenn die Box danach fragt.
- Die Daten werden **ohne Umwandlung gestreamt**, also mit kleinem Puffer statt die ganze Datei zwischenzuspeichern. Bricht der Sender ab, endet auch die Antwort an die Box.
- Nur eine aktive Verbindung pro Box: Kommt eine neue Anfrage derselben Box, wird die alte Upstream-Verbindung geschlossen.
- Fehlerfälle:
  - Sender nicht erreichbar → `502` mit Klartext-Log.
  - Unbekannte Box oder unbekanntes Preset → `404`.
- **Version 1 unterstützt:** direkte MP3- und AAC-Streams über http oder https, dazu einfache Playlists (`.m3u`, `.pls`: der erste Eintrag wird verwendet).
- **Später:** HLS (`.m3u8`), Ogg, FLAC per ffmpeg (optional, nur wenn installiert).

### 5.5 Preset-Sync
- Ziel-Inhalt pro Slot:
  ```xml
  <preset id="N"><ContentItem source="UPNP" location="http://<public_url>/stream/<deviceID>/N"
    sourceAccount="UPnPUserName" isPresetable="true"><itemName>Sendername</itemName>
    <containerArt>logo-url</containerArt></ContentItem></preset>
  ```
- **Vor dem ersten Überschreiben** werden die aktuellen Presets jeder Box als XML in `backups/<deviceID>-<zeitstempel>.xml` gesichert. Mit `strb restore` lassen sie sich zurückspielen.
- Abgleich beim Start, bei `presetsUpdated` von der Box und alle 30 Minuten. Geschrieben wird nur, was abweicht, danach wird nachgeprüft (`GET /presets`).
- Ändert sich `public_url` (z.B. weil der Mac eine neue IP hat), werden alle Presets automatisch neu geschrieben.

### 5.6 Stereo-Paar (Bose Stereo Link)
- Ein Paar erscheint als zwei Geräte mit gemeinsamer Gruppe (`GET :8090/getGroup`, Meldung `groupUpdated`). Der **Master** (Rolle `LEFT`, `masterDeviceId`) meldet die Tastendrücke und nimmt die UPnP-Befehle an. Die zweite Box spielt automatisch mit.
- `strb` steuert nur den Master. Die zweite Box wird erkannt und im Log genannt, aber nicht angefasst. Ihre Presets werden nicht umgeschrieben.
- Ein Paar ist nur funktionsfähig, wenn beide Boxen im Netz sind. Bei `GROUP_ERROR` loggt `strb` eine Warnung, z.B. „Paar Links+Rechts: rechte Box nicht erreichbar, Tasten funktionieren erst wieder, wenn sie zurück ist“. Sobald `GROUP_OK` kommt, folgt eine Entwarnung.
- Gruppen-Meldungen können veraltete IPs enthalten. Sie werden nie zur Adressierung verwendet.

---

## 6. Bedienung (CLI)

| Befehl | Zweck |
|---|---|
| `strb run` | Normalbetrieb (Vordergrund, Logs auf stdout) |
| `strb discover` | Gefundene Boxen auflisten (Name, Modell, IP, deviceID) |
| `strb presets [--box X]` | Aktuelle Presets der Boxen anzeigen |
| `strb sync` | Presets einmalig umschreiben (mit Backup), dann beenden |
| `strb play <box> <slot\|station>` | Sender testweise abspielen |
| `strb restore <backup-datei>` | Alte Presets zurückspielen |
| `strb listen <box>` | Rohe WebSocket-Meldungen anzeigen (Diagnose) |

- Logs: einzeilig und gut lesbar, z.B. `12:01:03 INFO  [Links] Taste 3 → SwissGroove ▶ ok (420 ms)`. Mit `--debug` kommen alle Rohmeldungen dazu.
- Beenden mit Ctrl+C: Verbindungen sauber schließen.

---

## 7. Zuverlässigkeit (Kernanforderungen)

Daran sind die bestehenden Lösungen bei dir gescheitert. Jede Regel hat einen Test (siehe Abschnitt 9).

| # | Problem in bestehenden Lösungen | Anforderung an strb |
|---|---|---|
| Z1 | Eine Box ist beim Start aus oder nicht erreichbar → der Teil für diese Box beendet sich endgültig | Pro Box ein **Supervisor**, der nie aufgibt. Neuer Versuch mit Backoff von 1 s bis 60 s, ohne Limit. |
| Z2 | Nach einem Stromausfall der Box bleibt der WebSocket scheinbar offen, kommt aber nie wieder | Alle 30 s ein WS-Ping, Antwort-Timeout 10 s. Zusätzlich wird die Verbindung **alle 10 min erneuert**: erst die neue öffnen, dann die alte schließen, damit kein Druck verloren geht. |
| Z3 | Die IP der Box ändert sich | Identität über die `deviceID`, Neusuche per Discovery (5.1). |
| Z4 | HTTPS-Streams scheitern mit „402 No URI supplied“ | Die Box bekommt immer nur `http://` vom Proxy (5.4). |
| Z5 | Ein Druck kommt doppelt an | Entprellen wie in 5.2. |
| Z6 | Langsame Abfragen blockieren die Auswertung der Meldungen | Ereignisse laufen über eine Queue in den Player. Der Listener wird nie blockiert. Kommt ein neuer Druck, bricht er einen noch laufenden Start ab („der letzte Druck gewinnt“). |
| Z7 | Ein Fehler eines Senders oder ungültiges XML bringen alles zum Absturz | Fehler bleiben auf diese Box und dieses Ereignis begrenzt. Auch eine Panic wird im Supervisor abgefangen. |
| Z8 | Presets der Box „verlieren“ sich (bekannt aus der Firmware) | Regelmäßiger Abgleich (5.5). |
| Z9 | Der Rechner hat eine neue IP | Presets werden automatisch neu geschrieben (5.5). Empfehlung: im Router eine feste IP für den Rechner reservieren. |
| Z10 | Ein Stereo-Paar ist halb offline, und die Tasten tun „einfach nichts“, ohne dass man erfährt, warum | Gruppenstatus überwachen und klar loggen (5.6). Die Box setzt den WebSocket zurück, wenn das Paar sich neu bildet → Neuverbinden (Z1). |

**Wichtig zum Betrieb:** `strb` muss laufen, damit die Tasten funktionieren. Wenn der Mac schläft, passiert beim Tastendruck nichts. Für den Dauerbetrieb bietet sich ein Gerät an, das immer läuft: dein Home-Assistant-Rechner, ein Raspberry Pi oder ein NAS. Das Go-Programm läuft dort genauso.

---

## 8. Ausliefern und Betrieb

- Ein Build pro System, ohne Laufzeit-Abhängigkeiten: `darwin/arm64`, `darwin/amd64`, `windows/amd64`, `linux/amd64`, `linux/arm64`.
- Version 1: im Terminal starten.
- Später:
  - macOS: Autostart per `launchd`, `strb install` legt die `.plist` an.
  - Windows: Dienst per `strb install`.
  - Docker-Image mit `network_mode: host`.
- Netzwerk: Der Rechner muss im selben Netz wie die Boxen sein. Port 8686 muss von den Boxen aus erreichbar sein. Die macOS-Firewall fragt beim ersten Start danach.

---

## 9. Tests und Qualität

- **Unit-Tests:** XML-Parser mit echten, aufgezeichneten Meldungen, DIDL-Erzeugung, Konfigurationsprüfung, Entprellen.
- **Simulierte Box** (`internal/fakespeaker`): Ein Test-Server, der :8090, :8080 und :8091 nachbildet. Damit testen wir automatisch:
  - Tastendruck → Abspielen
  - Box verschwindet und kommt wieder (Z1)
  - stummer WebSocket (Z2)
  - IP-Wechsel (Z3)
  - doppelter Druck (Z5)
  - langsamer Sender (Z6)
  - ungültiges XML (Z7)
- **Proxy-Tests** mit einem lokalen Test-Sender: http, https, Redirects, Playlists, Abbruch.
- **Abnahme an den echten Boxen** (manuelle Checkliste):
  1. Alle 6 Tasten am SoundTouch 10 spielen den richtigen Sender, mit Namen im Display.
  2. Sender in `strb.yaml` ändern und speichern → die Taste spielt den neuen Sender, ohne Neustart.
  3. Schnelles Umschalten zwischen den Tasten funktioniert.
  4. Box vom Strom trennen und wieder anschließen → die Tasten gehen ohne Neustart von `strb`.
  5. WLAN-Router neu starten → die Tasten gehen wieder.
  6. 24 h Dauerlauf ohne Ausfall.

---

## 10. Meilensteine

| | Inhalt | Ergebnis |
|---|---|---|
| **M0 Spike** ✅ | Am SoundTouch 10 klären, was die Box beim Druck meldet und ob sie den Link selbst spielt. Meldungen aufzeichnen. | Erledigt am 2026-11-23, siehe [M0-FINDINGS.md](M0-FINDINGS.md) |
| **M1 MVP** | Konfiguration (mit Übernahme im laufenden Betrieb), Sync mit Backup, Listener, Player, Proxy (http/https), CLI `run`/`discover`/`play`/`sync`/`restore` | Tasten funktionieren am Mac (SoundTouch 10) |
| **M2 Robust** | Supervisor, Z1–Z9 vollständig, simulierte Box mit Tests, 24-h-Test | „Läuft einfach“ |
| **M3 Komfort** | Autostart Mac/Windows, Windows-Build getestet, Docker | Dauerbetrieb |
| später | HLS/ffmpeg, kleine Web-Oberfläche, HA/MQTT, Cloud-Emulation für volle Anzeige | – |

---

## 11. Risiken

| Risiko | Gegenmaßnahme |
|---|---|
| Die Box meldet den Druck bei `UPNP`-Presets anders als erwartet | ✅ In M0 geklärt: Sie meldet ihn wie erwartet. |
| Anzeige zeigt „Track“ statt Radio und kein Logo | Kosmetisch. In M1 mit DIDL und `protocolInfo` experimentieren. |
| Die Cinemate verhält sich anders als die ST10 | Wird in M0 an beiden Geräten getestet |
| Ein Sender ändert seine URL | Klare Fehlermeldung im Log, die URL wird in der Konfiguration angepasst |
| Eine Firmware-Eigenheit blockiert Port 8686 oder verlangt einen bestimmten Content-Type | M0, außerdem lässt sich der Content-Type im Proxy erzwingen |
| Presets überschreiben geht schief | Backup und `strb restore` |

---

## 12. Entscheidungen (2026-11-23)

1. **Sender:** Wir starten mit der bisherigen HA-Belegung (2.3), auf Taste 3 mit 101 Smooth Jazz als Ersatz. Ändern geht jederzeit über die Konfiguration, ohne Neustart.
2. **Cinemate:** vorerst nicht im Umfang. Es wird nur der SoundTouch 10 angefasst.
3. **Betriebsort:** zuerst auf dem Mac, Dauerbetrieb später (M3).
4. **Name:** `soundtouch-radio-bridge`, Befehl `strb`.
5. **Repo:** öffentlich auf GitHub, MIT-Lizenz. Keine gerätespezifischen Daten im Repo.

---

## 13. Quellen

- Bose: SoundTouch End of Life – https://www.bose.com/soundtouch-end-of-life
- Bose SoundTouch Web API v1.1 (PDF, öffentlich gemacht 2026)
- sandervg/homeassistant-bose-soundtouch-bridge – https://github.com/sandervg/homeassistant-bose-soundtouch-bridge
- Air-Drift/Sound-Touch – https://github.com/Air-Drift/Sound-Touch
- gesellix Bose-SoundTouch Toolkit – https://gesellix.github.io/Bose-SoundTouch/
- deborahgu/soundcork, timvw/soundcork (Cloud-Emulation, nicht gewählt)
- TJGigs/Bose-SoundTouch-Hybrid (Cloud-Emulation + Music Assistant, nicht gewählt)
- radio-browser.info (Stream-URLs)
