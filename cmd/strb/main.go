// Command strb revives the preset buttons of Bose SoundTouch speakers.
package main

import (
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/bridge"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/config"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/discovery"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/logx"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/speaker"
	"github.com/coder/websocket"
)

// version is set at build time: -ldflags "-X main.version=v1.0.0".
var version = "dev"

const usage = `strb – SoundTouch Radio Bridge %s

Erweckt die Preset-Tasten von Bose-SoundTouch-Boxen wieder zum Leben.

Befehle:
  strb run                         Normalbetrieb (läuft, bis Ctrl+C)
  strb discover                    Boxen im Netz anzeigen
  strb presets [box]               gespeicherte Presets anzeigen
  strb sync                        Presets einmalig umschreiben (mit Sicherung)
  strb play <box> <taste|sender>   Sender testweise abspielen
  strb restore <datei> [box]       gesicherte Presets zurückspielen
  strb listen <box>                rohe Meldungen der Box anzeigen (Diagnose)
  strb version

<box> ist deviceID, Name oder IP-Adresse.

Optionen:
  --config <datei>   Konfiguration (Standard: ./strb.yaml)
  --debug            ausführliche Ausgabe
`

func main() {
	fs := flag.NewFlagSet("strb", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Konfigurationsdatei")
	debug := fs.Bool("debug", false, "ausführliche Ausgabe")
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, version) }

	// Allow flags before and after the command.
	var args []string
	rest := os.Args[1:]
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			os.Exit(2)
		}
		rest = fs.Args()
		if len(rest) > 0 {
			args = append(args, rest[0])
			rest = rest[1:]
		}
	}
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := logx.New(os.Stdout, level)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app := &app{log: log, cfgPath: *cfgPath}
	var err error
	switch cmd, a := args[0], args[1:]; cmd {
	case "run":
		err = app.run(ctx)
	case "discover":
		err = app.discover(ctx)
	case "presets":
		err = app.presets(ctx, a)
	case "sync":
		err = app.sync(ctx)
	case "play":
		err = app.play(ctx, a)
	case "restore":
		err = app.restore(ctx, a)
	case "listen":
		err = app.listen(ctx, a)
	case "version":
		fmt.Println("strb", version)
	case "help", "-h", "--help":
		fs.Usage()
	default:
		fmt.Fprintf(os.Stderr, "unbekannter Befehl %q\n\n", cmd)
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Fehler:", err)
		os.Exit(1)
	}
}

type app struct {
	log     *slog.Logger
	cfgPath string
}

func (a *app) config() (*config.Config, error) {
	p := a.cfgPath
	if p == "" {
		var err error
		if p, err = config.Find(); err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	return config.Load(abs)
}

func (a *app) bridge() (*bridge.Bridge, error) {
	cfg, err := a.config()
	if err != nil {
		return nil, err
	}
	return bridge.New(cfg, a.log, version), nil
}

func (a *app) run(ctx context.Context) error {
	b, err := a.bridge()
	if err != nil {
		return err
	}
	return b.Run(ctx)
}

func (a *app) discover(ctx context.Context) error {
	fmt.Println("Suche Boxen (3 s) …")
	found, err := discovery.Search(ctx, 3*time.Second)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return errors.New("keine SoundTouch-Box gefunden. Sind Mac und Boxen im selben Netz?")
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tMODELL\tIP\tDEVICE-ID\tFIRMWARE\tSTEREO-PAAR")
	for _, f := range found {
		pair := "–"
		gctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		if g, err := speaker.New(f.Host).Group(gctx); err == nil && g.Paired() {
			if g.MasterID == f.Info.DeviceID {
				pair = "Master (" + g.Status + ")"
			} else {
				pair = "zweite Box, Master " + g.MasterID
			}
		}
		cancel()
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", f.Info.Name, f.Info.Type, f.Host, f.Info.DeviceID, f.Info.Firmware(), pair)
	}
	tw.Flush()
	fmt.Println("\nFür strb.yaml unter speakers: die deviceID der Einzelboxen bzw. des Masters eines Paars eintragen.")
	return nil
}

// findBox resolves a deviceID, name or IP to (host, deviceID, name).
func (a *app) findBox(ctx context.Context, arg string) (string, string, string, error) {
	if net := regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`); net.MatchString(arg) {
		ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		info, err := speaker.New(arg).Info(ictx)
		if err != nil {
			return "", "", "", err
		}
		return arg, info.DeviceID, info.Name, nil
	}
	var hosts []string
	if cfg, err := a.config(); err == nil {
		for _, sp := range cfg.Speakers {
			if sp.Host != "" {
				hosts = append(hosts, sp.Host)
			}
		}
	}
	match := func(fs []discovery.Found) (discovery.Found, bool) {
		for _, f := range fs {
			if strings.EqualFold(f.Info.DeviceID, arg) || strings.EqualFold(f.Info.Name, arg) {
				return f, true
			}
		}
		return discovery.Found{}, false
	}
	if f, ok := match(discovery.Verify(ctx, hosts)); ok {
		return f.Host, f.Info.DeviceID, f.Info.Name, nil
	}
	found, _ := discovery.Search(ctx, 3*time.Second)
	if f, ok := match(found); ok {
		return f.Host, f.Info.DeviceID, f.Info.Name, nil
	}
	return "", "", "", fmt.Errorf("Box %q nicht gefunden (siehe `strb discover`)", arg)
}

func (a *app) presets(ctx context.Context, args []string) error {
	var targets []discovery.Found
	if len(args) == 1 {
		host, id, name, err := a.findBox(ctx, args[0])
		if err != nil {
			return err
		}
		targets = append(targets, discovery.Found{Host: host, Info: speaker.Info{DeviceID: id, Name: name}})
	} else {
		b, err := a.bridge()
		if err != nil {
			return err
		}
		targets = b.Speakers(ctx)
	}
	for _, f := range targets {
		p, err := bridge.Presets(ctx, f.Host)
		if err != nil {
			return err
		}
		fmt.Printf("%s (%s, %s)\n", f.Info.Name, f.Host, f.Info.DeviceID)
		for _, x := range p {
			fmt.Printf("  %d  %-30s %-10s %s\n", x.ID, x.Item.ItemName, x.Item.Source, x.Item.Location)
		}
	}
	return nil
}

func (a *app) sync(ctx context.Context) error {
	b, err := a.bridge()
	if err != nil {
		return err
	}
	return b.SyncOnce(ctx)
}

func (a *app) play(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("Aufruf: strb play <box> <taste 1–6 | sender>")
	}
	b, err := a.bridge()
	if err != nil {
		return err
	}
	host, id, _, err := a.findBox(ctx, args[0])
	if err != nil {
		return err
	}
	if slot, err := strconv.Atoi(args[1]); err == nil {
		if slot < 1 || slot > 6 {
			return errors.New("Taste muss 1–6 sein")
		}
		return b.PlayOnce(ctx, host, id, slot, "")
	}
	return b.PlayOnce(ctx, host, id, 0, args[1])
}

var backupName = regexp.MustCompile(`^([0-9A-Fa-f]{12})-`)

func (a *app) restore(ctx context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("Aufruf: strb restore <datei> [box]")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var doc struct {
		Presets []struct {
			ID    int    `xml:"id,attr"`
			Inner []byte `xml:",innerxml"`
		} `xml:"preset"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil || len(doc.Presets) == 0 {
		return fmt.Errorf("%s ist keine Preset-Sicherung", args[0])
	}
	target := ""
	if len(args) == 2 {
		target = args[1]
	} else if m := backupName.FindStringSubmatch(filepath.Base(args[0])); m != nil {
		target = m[1]
	} else {
		return errors.New("aus dem Dateinamen geht die Box nicht hervor; bitte als zweites Argument angeben")
	}
	host, _, name, err := a.findBox(ctx, target)
	if err != nil {
		return err
	}
	sp := speaker.New(host)
	for _, p := range doc.Presets {
		if err := sp.StoreRawPreset(ctx, p.ID, p.Inner); err != nil {
			return fmt.Errorf("Preset %d: %w", p.ID, err)
		}
		fmt.Printf("Preset %d zurückgespielt\n", p.ID)
	}
	fmt.Printf("Fertig: %d Presets auf %s. Hinweis: Ein laufendes `strb run` schreibt sie beim nächsten Abgleich wieder um.\n", len(doc.Presets), name)
	return nil
}

func (a *app) listen(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("Aufruf: strb listen <box>")
	}
	host, _, name, err := a.findBox(ctx, args[0])
	if err != nil {
		return err
	}
	c, _, err := websocket.Dial(ctx, "ws://"+host+":8080", &websocket.DialOptions{Subprotocols: []string{"gabbo"}})
	if err != nil {
		return err
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	fmt.Printf("verbunden mit %s (%s), Ctrl+C zum Beenden\n", name, host)
	for {
		_, msg, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		fmt.Println(time.Now().Format("15:04:05.000"), string(msg))
	}
}
