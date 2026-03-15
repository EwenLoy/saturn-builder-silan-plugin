package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var version = "dev"

func envOr(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func joinUpstream(base string, req *http.Request) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if req != nil && req.URL != nil {
		u.Path = req.URL.Path
		u.RawQuery = req.URL.RawQuery
		u.Fragment = req.URL.Fragment
	}
	return u.String(), nil
}

func copyPump(ctx context.Context, dst, src *websocket.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		mt, r, err := src.NextReader()
		if err != nil {
			return err
		}

		w, err := dst.NextWriter(mt)
		if err != nil {
			return err
		}
		_, cErr := io.Copy(w, r)
		_ = w.Close()
		if cErr != nil && cErr != io.EOF {
			return cErr
		}
	}
}

func main() {
	listenAddr := envOr("SATURN_BUILDER_LISTEN", "127.0.0.1:8787")
	upstreamBase := envOr("SATURN_BUILDER_UPSTREAM", "wss://build.construct.net/")

	log.SetFlags(0)
	log.Printf("[SaturnBuilderProxy] v=%s listen=%s upstream=%s", version, listenAddr, upstreamBase)

	u := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "build.construct.net"},
		Proxy:            http.ProxyFromEnvironment,
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upURL, err := joinUpstream(upstreamBase, r)
		if err != nil {
			log.Printf("[SaturnBuilderProxy] upstream url parse error: %v", err)
			http.Error(w, "bad upstream", http.StatusBadGateway)
			return
		}

		clientConn, err := u.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[SaturnBuilderProxy] upgrade failed: %v", err)
			return
		}

		headers := http.Header{}
		if p := r.Header.Get("Sec-WebSocket-Protocol"); p != "" {
			headers.Set("Sec-WebSocket-Protocol", p)
		}
		if ua := r.Header.Get("User-Agent"); ua != "" {
			headers.Set("User-Agent", ua)
		}

		upConn, resp, err := dialer.Dial(upURL, headers)
		if err != nil {
			status := ""
			if resp != nil {
				status = resp.Status
			}
			log.Printf("[SaturnBuilderProxy] upstream dial failed: %v %s", err, status)
			_ = clientConn.Close()
			return
		}

		log.Printf("[SaturnBuilderProxy] connected: %s -> %s", r.URL.String(), upURL)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errCh := make(chan error, 2)
		go func() { errCh <- copyPump(ctx, upConn, clientConn) }()
		go func() { errCh <- copyPump(ctx, clientConn, upConn) }()

		_ = <-errCh
		cancel()
		_ = clientConn.Close()
		_ = upConn.Close()
	})

	s := &http.Server{Addr: listenAddr, Handler: h}
	if err := s.ListenAndServe(); err != nil {
		fmt.Println(err)
	}
}
