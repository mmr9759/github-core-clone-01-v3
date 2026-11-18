// Forge entrypoint: listen on an ephemeral port, register with the trial
// environment, serve until the environment reports done, then exit 0.
// Without MAKEFUN_ENV_URL it simply serves (local testing).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	port := flag.Int("port", 0, "port to listen on (0 = ephemeral)")
	token := flag.String("token", "", "shop token (overrides MAKEFUN_SHOP_TOKEN)")
	flag.Parse()
	tok := *token
	if tok == "" {
		tok = os.Getenv("MAKEFUN_SHOP_TOKEN")
	}
	viewer := viewerLogin()
	st := newStore(viewer)
	srv := &server{st: st, token: tok}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", *port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: listen: %v\n", err)
		os.Exit(1)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(os.Stderr, "forge: listening on %d as %s\n", actualPort, viewer)
	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL != "" {
		for i := 0; i < 60; i++ {
			if _, _, err := httpRaw("POST", envURL+"/register", fmt.Sprintf(`{"port":%d}`, actualPort)); err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		go func() {
			delay := 250 * time.Millisecond
			for {
				_, body, err := httpRaw("GET", envURL+"/done", "")
				if err == nil {
					var done struct {
						Done bool `json:"done"`
					}
					if json.Unmarshal(body, &done) == nil && done.Done {
						os.Exit(0)
					}
				}
				time.Sleep(delay)
				if delay < 2*time.Second {
					delay *= 2
				}
			}
		}()
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		os.Exit(0)
	}()
	serveListener(ln, srv)
}

// httpRaw performs one minimal HTTP/1.1 request over raw TCP and returns the
// status code and body. No net/http: keeps the static binary small.
func httpRaw(method, rawURL, body string) (int, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	uri := u.RequestURI()
	if uri == "" {
		uri = "/"
	}
	var sb strings.Builder
	sb.WriteString(method + " " + uri + " HTTP/1.1\r\n")
	sb.WriteString("Host: " + u.Host + "\r\n")
	sb.WriteString("Content-Type: application/json\r\n")
	sb.WriteString(fmt.Sprintf("Content-Length: %d\r\n", len(body)))
	sb.WriteString("Connection: close\r\n\r\n")
	sb.WriteString(body)
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		return 0, nil, err
	}
	resp, err := io.ReadAll(conn)
	if err != nil {
		return 0, nil, err
	}
	head, respBody, _ := strings.Cut(string(resp), "\r\n\r\n")
	lines := strings.SplitN(head, "\r\n", 2)
	if len(lines) == 0 {
		return 0, nil, simpleError("empty response")
	}
	var status int
	fmt.Sscanf(lines[0], "HTTP/1.1 %d", &status)
	return status, []byte(respBody), nil
}
