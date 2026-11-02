package bridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hamhub-app/plex-local-bridge/internal/config"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "plb-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "control.sock")
}

func TestServerLifecycle(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/identity":
			io.WriteString(w, `<MediaContainer claimed="1"/>`)
		case "/library/sections":
			w.WriteHeader(401)
		default:
			io.WriteString(w, "media")
		}
	}))
	defer backend.Close()
	c := config.Default()
	c.UpstreamURL = backend.URL
	c.ListenPort = freePort(t)
	socket := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (&Server{Config: c, Socket: socket, Logger: quietLogger()}).Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for Health(ctx, socket) != nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := HealthMatches(ctx, socket, c); err != nil {
		t.Fatal(err)
	}
	changed := c
	changed.ListenPort++
	if err := HealthMatches(ctx, socket, changed); err == nil {
		t.Fatal("accepted proxy port without administrator approval")
	}
	changed = c
	changed.UpstreamURL = "http://127.0.0.1:1"
	if err := HealthMatches(ctx, socket, changed); err == nil {
		t.Fatal("accepted changed proxy upstream")
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(c.ListenPort) + "/media")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "media" {
		t.Fatalf("response=%q %v", body, err)
	}
	idle, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(c.ListenPort))
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("control socket remains: %v", err)
	}
	idle.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := idle.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle connection survived shutdown")
	}
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(c.ListenPort))
	if err != nil {
		t.Fatal("listener was not released:", err)
	}
	listener.Close()
}

func TestServerDoesNotDeleteControlFile(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/identity" {
			io.WriteString(w, `<MediaContainer claimed="1"/>`)
		} else {
			w.WriteHeader(401)
		}
	}))
	defer backend.Close()
	c := config.Default()
	c.UpstreamURL = backend.URL
	c.ListenPort = freePort(t)
	socket := socketPath(t)
	os.WriteFile(socket, []byte("keep"), 0600)
	if err := (&Server{Config: c, Socket: socket, Logger: quietLogger()}).Run(context.Background()); err == nil {
		t.Fatal("accepted occupied socket path")
	}
	data, _ := os.ReadFile(socket)
	if string(data) != "keep" {
		t.Fatal("deleted unrelated file")
	}
}

func TestTLSTunnelPreservesCertificateAndHeaders(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "original.plex.direct" || r.Header.Get("X-Forwarded-For") != "203.0.113.5" {
			t.Error("encrypted HTTP headers were altered")
		}
		io.WriteString(w, "secure media")
	}))
	defer backend.Close()
	clientSide, proxySide := net.Pipe()
	defer clientSide.Close()
	done := make(chan struct{})
	go func() { (&Server{}).tunnel(context.Background(), proxySide, parsed(t, backend.URL).Host); close(done) }()
	pool := x509.NewCertPool()
	pool.AddCert(backend.Certificate())
	client := tls.Client(clientSide, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	client.SetDeadline(time.Now().Add(3 * time.Second))
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(client.ConnectionState().PeerCertificates[0].Raw, backend.Certificate().Raw) {
		t.Fatal("certificate changed")
	}
	fmt.Fprint(client, "GET / HTTP/1.1\r\nHost: original.plex.direct\r\nX-Forwarded-For: 203.0.113.5\r\nConnection: close\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	client.Close()
	if err != nil || string(body) != "secure media" {
		t.Fatalf("TLS body=%q %v", body, err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("TLS tunnel leaked after disconnect")
	}
}
