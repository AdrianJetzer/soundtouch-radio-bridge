// Package discovery finds SoundTouch speakers on the local network via SSDP.
package discovery

import (
	"context"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/speaker"
)

// Found is one speaker that answered.
type Found struct {
	Host string
	Info speaker.Info
}

const search = "M-SEARCH * HTTP/1.1\r\n" +
	"HOST: 239.255.255.250:1900\r\n" +
	"MAN: \"ssdp:discover\"\r\n" +
	"MX: 2\r\n" +
	"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n"

// Search sends SSDP queries on every suitable IPv4 interface, waits for
// answers until wait has passed, and keeps the hosts that answer /info
// like a SoundTouch speaker. Results are de-duplicated by deviceID.
func Search(ctx context.Context, wait time.Duration) ([]Found, error) {
	hosts := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}

	for _, ip := range localIPv4s() {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(wait))
			for i := 0; i < 2; i++ {
				_, _ = conn.WriteTo([]byte(search), dst)
			}
			go func() { // the second burst catches speakers that missed the first
				time.Sleep(400 * time.Millisecond)
				_, _ = conn.WriteTo([]byte(search), dst)
			}()
			buf := make([]byte, 2048)
			for {
				n, from, err := conn.ReadFromUDP(buf)
				if err != nil {
					return
				}
				h := locationHost(string(buf[:n]))
				if h == "" {
					h = from.IP.String()
				}
				mu.Lock()
				hosts[h] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	var list []string
	for h := range hosts {
		list = append(list, h)
	}
	return Verify(ctx, list), nil
}

// Verify asks each host for /info and returns the SoundTouch speakers.
func Verify(ctx context.Context, hosts []string) []Found {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = map[string]Found{}
	)
	for _, h := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			info, err := speaker.New(h).Info(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			out[info.DeviceID] = Found{Host: h, Info: info}
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	res := make([]Found, 0, len(out))
	for _, f := range out {
		res = append(res, f)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Info.Name < res[j].Info.Name })
	return res
}

func locationHost(resp string) string {
	for _, line := range strings.Split(resp, "\r\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), "location") {
			if u, err := url.Parse(strings.TrimSpace(v)); err == nil {
				return u.Hostname()
			}
		}
	}
	return ""
}

// localIPv4s returns the IPv4 addresses of up, multicast-capable, non-loopback
// interfaces, skipping typical virtual ones.
func localIPv4s() []net.IP {
	var ips []net.IP
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagMulticast == 0 {
			continue
		}
		name := strings.ToLower(ifc.Name)
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "tailscale") || strings.HasPrefix(name, "bridge") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				ips = append(ips, n.IP.To4())
			}
		}
	}
	return ips
}

// LocalIPFor returns the local address used to reach host, i.e. the address
// the speaker can use to reach us.
func LocalIPFor(host string) (string, error) {
	c, err := net.Dial("udp4", net.JoinHostPort(host, "9"))
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String(), nil
}
