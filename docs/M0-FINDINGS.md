# M0 – Befunde am echten Gerät

| | |
|---|---|
| Datum | 2026-11-23 |
| Geräte | 2× SoundTouch 10 als Stereo-Paar (Bose Stereo Link), Firmware 27.0.6.46330 |
| Werkzeug | `cmd/spike` (listen, backup, serve, store, play) |
| Ergebnis | **Ansatz bestätigt.** Tastendruck → Ereignis → UPnP-Start → Proxy → Musik läuft |

Geräte-IDs und IPs sind in diesem Dokument ersetzt (`<MASTER>`, `<SLAVE>`, `<HOST>`).

## 1. Tastendruck-Ereignisse

Die Box sendet die Meldungen ungefragt über `ws://<box>:8080` (Subprotokoll `gabbo`). Eine Anmeldung oder ein Abo ist nicht nötig. Bei jedem Verbindungsaufbau kommt zuerst:

```xml
<SoundTouchSdkInfo serverVersion="4" serverBuild="trunk r46330 v4 epdbuild hepdswbld04" />
```

### Preset-Taste N (TuneIn-Preset, Cloud tot)

```xml
<updates deviceID="<MASTER>"><nowSelectionUpdated><preset id="1"><ContentItem source="TUNEIN" type="stationurl" location="/v1/playback/station/s142689" ...><itemName>…</itemName>…</ContentItem></preset></nowSelectionUpdated></updates>
<userActivityUpdate deviceID="<MASTER>" />
<updates …><nowPlayingUpdated><nowPlaying … source="INVALID_SOURCE">…</nowPlaying></nowPlayingUpdated></updates>
<updates …><nowPlayingUpdated><nowPlaying … source="STANDBY">…</nowPlaying></nowPlayingUpdated></updates>
```

Die Box versucht, den TuneIn-Sender zu spielen. Das scheitert (`INVALID_SOURCE`), und innerhalb von ca. 50 ms fällt sie zurück auf `STANDBY`.

### Preset-Taste N (umgeschrieben auf `UPNP` mit HTTP-Link)

```xml
<updates deviceID="<MASTER>"><nowSelectionUpdated><preset id="6"><ContentItem source="UPNP" location="http://<HOST>:8686/stream/<MASTER>/6" sourceAccount="UPnPUserName" isPresetable="true"><itemName>Chilltrax</itemName></ContentItem></preset></nowSelectionUpdated></updates>
<userActivityUpdate deviceID="<MASTER>" />
<errorUpdate deviceID="<MASTER>"><error value="1036" name="UNABLE_TO_PROCESS_NOT_LOGGED_IN" severity="Unrecoverable">UpnpRcvdContentItemInWrongState</error></errorUpdate>
```

- Das Ereignis kommt zuverlässig, ca. 15–20 ms nach dem Druck.
- **Die Box spielt den UPNP-Link nicht selbst ab** (Fehler 1036). Der aktive Start per UPnP ist also **Pflicht**, kein Sicherheitsnetz.
- Ein Druck erzeugt genau **eine** `nowSelectionUpdated`-Meldung. Doppelte Meldungen haben wir nicht beobachtet, Entprellen bleibt trotzdem drin.

### Power-Taste

```xml
<updates …><nowSelectionUpdated><preset id="0"><ContentItem source="INVALID_SOURCE" type="DO_NOT_RESUME" location="unplayable location" sourceAccount="UPnPUserName" isPresetable="false" /></preset></nowSelectionUpdated></updates>
```

→ `preset id="0"` muss **ignoriert** werden.

### Weitere beobachtete Meldungen

- `volumeUpdated` (Lautstärke-Tasten, je Schritt eine Meldung)
- `connectionStateUpdated`, `balanceUpdated`
- `userActivityUpdate` bei jeder Bedienung
- `groupUpdated` (Stereo-Paar, siehe 3)

## 2. Abspielen per UPnP

- Steuer-URL: `http://<box>:8091/AVTransport/Control`, wie in der Gerätebeschreibung `:8091/XD/BO5EBO5E-F00D-F00D-FEED-<deviceID>.xml`.
- `Stop` → `SetAVTransportURI` (mit DIDL-Lite) → `Play`: alle drei mit `200 OK`, auch aus dem Standby heraus.
- Danach meldet `/now_playing` `source="UPNP"`, `playStatus=PLAY_STATE`, `itemName` = DIDL-Titel. Allerdings steht dort `streamType=TRACK_ONDEMAND` und `art=SHOW_DEFAULT_IMAGE`.
- Die Box holt den Stream mit `GET`, `User-Agent: Bose/7.22.0`, `Accept: */*`. Bei diesem Abruf schickte sie **keinen** `Icy-MetaData`- und **keinen** `Range`-Header.
- HTTPS-Sender über den Proxy: funktioniert (Chilltrax, `https://streamssl.chilltrax.com/stream`).

## 3. Stereo-Paar (Bose Stereo Link)

- Das Paar ist eine „Gruppe“ (`groupUpdated`). `getZone` ist leer, die Zone ist dafür das falsche Konzept.
- **Master** ist die Box mit der Rolle `LEFT`. Nur der Master meldet Tastendrücke und nimmt UPnP-Befehle an.
- **Fehlt die zweite Box im Netz** (`<status>GROUP_ERROR</status>`), ist der Master praktisch tot:
  - kein Ereignis bei Preset- oder Power-Taste
  - `/now_playing` bleibt `STANDBY`, obwohl das Display angeht
  - Benachrichtigungen per API (Lautstärke, Ton) funktionieren weiter
  - Als die zweite Box zurückkam, setzte der Master die WebSocket-Verbindung zurück (`connection reset by peer`). Danach kam `GROUP_ERROR`, kurz später `GROUP_OK`, und alles funktionierte.
- Die Gruppen-Meldung enthielt zunächst **veraltete IPs** aus einem früheren Netz (`192.168.1.x`). Die Box hat sie selbst korrigiert. → Wir dürfen uns nie auf IPs aus Box-Meldungen verlassen.

## 4. Sonstiges

- `.local`-Namen der Boxen lösen über curl/Go nicht zuverlässig auf. Die Router-DNS-Namen schon, ebenso direkte IPs.
- SomaFM antwortet auf einen `Range`-Header mit `403`.
- TuneIn liefert für manche Sender nur noch `nostream.enUS.mp3` (z.B. „Capital Jazz Lounge“).

## 5. Folgen für die Spec (v0.3)

1. Player-Start ist Pflicht (3, 5.3).
2. `preset id="0"` ignorieren (5.2).
3. Stereo-Paar unterstützen: nur der Master wird gesteuert, `GROUP_ERROR` wird im Log deutlich gemeldet (neu 5.6, Z10).
4. WebSocket-Reset durch die Box ist normal → Neuverbinden (Z1, Z2) ist bestätigt nötig.
5. Offen für M1: Lässt sich die Anzeige als „Radio“ statt „Track“ und mit Logo verbessern (DIDL, `protocolInfo`, ICY)? Kosmetisch, kein Blocker.
