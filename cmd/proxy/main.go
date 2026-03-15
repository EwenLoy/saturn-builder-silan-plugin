package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
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

func pipeConns(a, b net.Conn) {
	if a == nil || b == nil {
		return
	}

	done := make(chan struct{}, 2)

	go func() {
		n, err := io.Copy(a, b)
		log.Printf("[SaturnBuilderProxy] tunnel b->a done bytes=%d err=%v", n, err)
		_ = a.Close()
		_ = b.Close()
		done <- struct{}{}
	}()

	go func() {
		n, err := io.Copy(b, a)
		log.Printf("[SaturnBuilderProxy] tunnel a->b done bytes=%d err=%v", n, err)
		_ = a.Close()
		_ = b.Close()
		done <- struct{}{}
	}()

	<-done
}

func main() {
	listenAddr := envOr("SATURN_BUILDER_LISTEN", "127.0.0.1:8787")
	upstreamBase := envOr("SATURN_BUILDER_UPSTREAM", "wss://build.construct.net/")

	log.SetFlags(0)
	log.Printf("[SaturnBuilderProxy] v=%s listen=%s upstream=%s", version, listenAddr, upstreamBase)

	u := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	baseDialer := websocket.Dialer{
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

		reqProto := r.Header.Get("Sec-WebSocket-Protocol")
		reqExt := r.Header.Get("Sec-WebSocket-Extensions")
		wantsCompression := strings.Contains(strings.ToLower(reqExt), "permessage-deflate")
		cookiePresent := r.Header.Get("Cookie") != ""
		originPresent := r.Header.Get("Origin") != ""

		headers := http.Header{}
		if p := r.Header.Get("Sec-WebSocket-Protocol"); p != "" {
			headers.Set("Sec-WebSocket-Protocol", p)
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			headers.Set("Origin", origin)
		}
		if cookie := r.Header.Get("Cookie"); cookie != "" {
			headers.Set("Cookie", cookie)
		}
		if ua := r.Header.Get("User-Agent"); ua != "" {
			headers.Set("User-Agent", ua)
		}

		dialer := baseDialer
		dialer.EnableCompression = wantsCompression

		upConn, resp, err := dialer.Dial(upURL, headers)
		if err != nil {
			status := ""
			if resp != nil {
				status = resp.Status
			}
			log.Printf("[SaturnBuilderProxy] upstream dial failed: %v %s", err, status)
			return
		}
		selectedProto := upConn.Subprotocol()
		selectedExt := ""
		if resp != nil {
			selectedExt = resp.Header.Get("Sec-WebSocket-Extensions")
		}
		upstreamCompression := strings.Contains(strings.ToLower(selectedExt), "permessage-deflate")

		respHeaders := http.Header{}
		if selectedProto != "" {
			respHeaders.Set("Sec-WebSocket-Protocol", selectedProto)
		}

		upgrader := u
		// Important: for raw tunneling we must not negotiate different extensions
		// on client vs upstream. Only enable compression if upstream also negotiated it.
		upgrader.EnableCompression = upstreamCompression

		clientConn, err := upgrader.Upgrade(w, r, respHeaders)
		if err != nil {
			log.Printf("[SaturnBuilderProxy] client upgrade failed: %v", err)
			_ = upConn.Close()
			return
		}

		log.Printf(
			"[SaturnBuilderProxy] connected: %s -> %s req_subprotocol=%q selected_subprotocol=%q req_ext=%q selected_ext=%q wants_compression=%v upstream_compression=%v cookie=%v origin=%v",
			r.URL.String(),
			upURL,
			reqProto,
			selectedProto,
			reqExt,
			selectedExt,
			wantsCompression,
			upstreamCompression,
			cookiePresent,
			originPresent,
		)

		// After both handshakes are complete, tunnel raw websocket frames.
		// This preserves masking + control frames (ping/pong/close) end-to-end.
		pipeConns(clientConn.UnderlyingConn(), upConn.UnderlyingConn())
		_ = clientConn.Close()
		_ = upConn.Close()
	})

	s := &http.Server{Addr: listenAddr, Handler: h}
	if err := s.ListenAndServe(); err != nil {
		fmt.Println(err)
	}
}
