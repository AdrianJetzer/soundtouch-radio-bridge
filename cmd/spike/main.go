// Command spike is a throwaway diagnostic tool for milestone M0.
// It records raw WebSocket notifications of a SoundTouch speaker and
// can back up its presets. It does not change anything on the speaker.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/coder/websocket"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: spike listen|backup <ip> [-for 10m]")
		os.Exit(2)
	}
	cmd, ip := os.Args[1], os.Args[2]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dur := fs.Duration("for", 10*time.Minute, "how long to listen")
	_ = fs.Parse(os.Args[3:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch cmd {
	case "listen":
		ctx, cancel := context.WithTimeout(ctx, *dur)
		defer cancel()
		err = listen(ctx, ip)
	case "backup":
		err = backup(ctx, ip)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func listen(ctx context.Context, ip string) error {
	if err := os.MkdirAll("captures", 0o755); err != nil {
		return err
	}
	name := filepath.Join("captures", fmt.Sprintf("ws-%s-%s.log", ip, time.Now().Format("20060102-150405")))
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	defer f.Close()
	out := io.MultiWriter(os.Stdout, f)

	conn, _, err := websocket.Dial(ctx, "ws://"+ip+":8080", &websocket.DialOptions{Subprotocols: []string{"gabbo"}})
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	fmt.Fprintf(out, "# %s connected to %s (subprotocol %q), writing %s\n", ts(), ip, conn.Subprotocol(), name)

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			fmt.Fprintf(out, "# %s closed: %v\n", ts(), err)
			return err
		}
		fmt.Fprintf(out, "%s %s\n", ts(), data)
	}
}

func backup(ctx context.Context, ip string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ip+":8090/presets", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := os.MkdirAll("backups", 0o755); err != nil {
		return err
	}
	name := filepath.Join("backups", fmt.Sprintf("presets-%s-%s.xml", ip, time.Now().Format("20060102-150405")))
	if err := os.WriteFile(name, body, 0o644); err != nil {
		return err
	}
	fmt.Println("saved", name)
	return nil
}

func ts() string { return time.Now().Format("15:04:05.000") }
