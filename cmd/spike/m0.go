package main

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// serve runs a minimal stream proxy: every GET /stream/... is relayed from
// upstream (http or https) to the speaker as plain http. Every request is
// logged with its headers so we learn what the speaker sends.
func serve(ctx context.Context, addr, upstream string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("REQ %s %s from %s", r.Method, r.URL.String(), r.RemoteAddr)
		for k, v := range r.Header {
			log.Printf("    %s: %s", k, strings.Join(v, ", "))
		}
		if !strings.HasPrefix(r.URL.Path, "/stream/") {
			http.NotFound(w, r)
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream, nil)
		req.Header.Set("User-Agent", "strb-spike/0.1")
		req.Header.Set("Accept", "*/*")
		if r.Header.Get("Icy-MetaData") != "" {
			req.Header.Set("Icy-MetaData", r.Header.Get("Icy-MetaData"))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("    upstream error: %v", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		log.Printf("    upstream %s, Content-Type %s", resp.Status, resp.Header.Get("Content-Type"))
		for _, h := range []string{"Content-Type", "icy-name", "icy-br", "icy-genre", "icy-metaint"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if r.Method == http.MethodHead {
			return
		}
		start := time.Now()
		n, err := io.Copy(flushWriter{w}, resp.Body)
		log.Printf("    stream ended after %s, %d bytes, err=%v", time.Since(start).Round(time.Second), n, err)
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { <-ctx.Done(); srv.Close() }()
	log.Printf("serving on %s, upstream %s", addr, upstream)
	return srv.ListenAndServe()
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// storePreset writes one preset slot as a UPNP content item pointing at location.
func storePreset(ctx context.Context, ip, slot, location, name string) error {
	body := fmt.Sprintf(`<preset id="%s"><ContentItem source="UPNP" location="%s" sourceAccount="UPnPUserName" isPresetable="true"><itemName>%s</itemName></ContentItem></preset>`,
		slot, html.EscapeString(location), html.EscapeString(name))
	return post(ctx, "http://"+ip+":8090/storePreset", "application/xml", "", body)
}

// upnpPlay runs Stop, SetAVTransportURI, Play against the speaker's AVTransport.
func upnpPlay(ctx context.Context, ip, uri, title string) error {
	didl := fmt.Sprintf(`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/"><item id="0" parentID="-1" restricted="1"><dc:title>%s</dc:title><upnp:class>object.item.audioItem.audioBroadcast</upnp:class><res protocolInfo="http-get:*:audio/mpeg:*">%s</res></item></DIDL-Lite>`,
		html.EscapeString(title), html.EscapeString(uri))
	ctl := "http://" + ip + ":8091/AVTransport/Control"
	steps := []struct{ action, args string }{
		{"Stop", ""},
		{"SetAVTransportURI", "<CurrentURI>" + html.EscapeString(uri) + "</CurrentURI><CurrentURIMetaData>" + html.EscapeString(didl) + "</CurrentURIMetaData>"},
		{"Play", "<Speed>1</Speed>"},
	}
	for _, s := range steps {
		env := `<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:` +
			s.action + ` xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"><InstanceID>0</InstanceID>` + s.args + `</u:` + s.action + `></s:Body></s:Envelope>`
		err := post(ctx, ctl, `text/xml; charset="utf-8"`, `"urn:schemas-upnp-org:service:AVTransport:1#`+s.action+`"`, env)
		fmt.Printf("%s: err=%v\n", s.action, err)
		if err != nil && s.action != "Stop" {
			return err
		}
	}
	return nil
}

func post(ctx context.Context, url, ctype, soapAction, body string) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", ctype)
	if soapAction != "" {
		req.Header.Set("SOAPAction", soapAction)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("  %s -> %s %s\n", url, resp.Status, strings.TrimSpace(string(b)))
	if resp.StatusCode >= 300 || bytes.Contains(b, []byte("<errors")) {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}
