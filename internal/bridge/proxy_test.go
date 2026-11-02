package bridge

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func parsed(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestProxyPreservesPlexSemantics(t *testing.T) {
	var upstreamHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != upstreamHost {
			t.Errorf("Host=%q, want %q", r.Host, upstreamHost)
		}
		for _, key := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Forwarded-Server", "X-Real-IP", "X-Client-IP", "True-Client-IP"} {
			if r.Header.Get(key) != "" {
				t.Errorf("forwarded identity leaked: %s", key)
			}
		}
		if r.Header.Get("X-Plex-Token") != "test-token" {
			http.Error(w, "Unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/media/file":
			if r.Header.Get("Range") != "bytes=2-5" || r.URL.Query().Get("name") != "a/b c" {
				t.Error("Range/query changed")
			}
			w.Header().Set("Content-Range", "bytes 2-5/10")
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("ETag", `"file-version"`)
			w.WriteHeader(206)
			io.WriteString(w, "2345")
		case "/denied":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(403)
			io.WriteString(w, "Plex playback denied by server.")
		case "/invalid-range":
			w.Header().Set("Content-Range", "bytes */10")
			w.WriteHeader(416)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	upstreamHost = parsed(t, upstream.URL).Host
	defer upstream.Close()
	proxy := NewProxy(parsed(t, upstream.URL), quietLogger())
	defer proxy.Transport.(*http.Transport).CloseIdleConnections()
	server := httptest.NewServer(proxy)
	defer server.Close()
	for _, test := range []struct {
		path, token, body string
		status            int
	}{
		{"/media/file?name=a%2Fb%20c", "test-token", "2345", 206},
		{"/denied", "test-token", "Plex playback denied by server.", 403},
		{"/invalid-range", "test-token", "", 416},
		{"/media/file", "", "Unauthorized\n", 401},
	} {
		t.Run(test.path+test.token, func(t *testing.T) {
			req, _ := http.NewRequest("GET", server.URL+test.path, nil)
			req.Host = "public.example:32400"
			req.Header.Set("X-Plex-Token", test.token)
			req.Header.Set("Range", "bytes=2-5")
			for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Forwarded-Server", "X-Real-IP", "X-Client-IP", "True-Client-IP"} {
				req.Header.Set(h, "203.0.113.1")
			}
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != test.status || string(body) != test.body {
				t.Fatalf("status=%d body=%q", resp.StatusCode, body)
			}
			if test.status == 206 && (resp.Header.Get("Content-Range") != "bytes 2-5/10" || resp.Header.Get("ETag") != `"file-version"`) {
				t.Fatal("Range metadata changed")
			}
			if test.status == 416 && resp.Header.Get("Content-Range") != "bytes */10" {
				t.Fatal("416 metadata changed")
			}
		})
	}
}

func TestCheckUpstreamRequiresClaimAndAuthentication(t *testing.T) {
	for _, test := range []struct {
		name, identity string
		sections       int
		valid          bool
	}{
		{"json", `{"MediaContainer":{"claimed":true}}`, 401, true},
		{"xml", `<MediaContainer claimed="1"/>`, 403, true},
		{"unclaimed", `<MediaContainer claimed="0"/>`, 401, false},
		{"anonymous library", `<MediaContainer claimed="1"/>`, 200, false},
		{"redirect", `<MediaContainer claimed="1"/>`, 302, false},
		{"invalid", strings.Repeat("x", 65537), 401, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Plex-Token") != "" || r.URL.RawQuery != "" {
					t.Error("credential in preflight")
				}
				if r.URL.Path == "/identity" {
					io.WriteString(w, test.identity)
					return
				}
				if r.URL.Path != "/library/sections" {
					t.Error("unexpected media access")
				}
				w.WriteHeader(test.sections)
			}))
			defer server.Close()
			if err := CheckUpstream(context.Background(), parsed(t, server.URL)); (err == nil) != test.valid {
				t.Fatalf("CheckUpstream()=%v", err)
			}
		})
	}
}

func TestProxyStreamsAndCancelsUpstream(t *testing.T) {
	canceled := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer backend.Close()
	proxy := NewProxy(parsed(t, backend.URL), quietLogger())
	defer proxy.Transport.(*http.Transport).CloseIdleConnections()
	server := httptest.NewServer(proxy)
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 5)
	if _, err := io.ReadFull(response.Body, chunk); err != nil || string(chunk) != "first" {
		t.Fatalf("first chunk: %q %v", chunk, err)
	}
	response.Body.Close()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled playback kept reading upstream")
	}
}

func TestProxyWebSocketUpgrade(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		rw.Flush()
		data := make([]byte, 5)
		if _, err := io.ReadFull(rw, data); err != nil {
			return
		}
		rw.Write(data)
		rw.Flush()
	}))
	defer backend.Close()
	proxy := NewProxy(parsed(t, backend.URL), quietLogger())
	defer proxy.Transport.(*http.Transport).CloseIdleConnections()
	server := httptest.NewServer(proxy)
	defer server.Close()
	c, err := net.DialTimeout("tcp", parsed(t, server.URL).Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(c, "GET /:/websockets/notifications HTTP/1.1\r\nHost: plex\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	reader := bufio.NewReader(c)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 101 {
		t.Fatal(response.Status)
	}
	fmt.Fprint(c, "hello")
	result := make([]byte, 5)
	if _, err := io.ReadFull(reader, result); err != nil || string(result) != "hello" {
		t.Fatalf("upgrade echo: %q %v", result, err)
	}
}

func TestProxyPreservesQueryTokenAndEncodedPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("X-Plex-Token") != "query-token" || r.URL.Query().Get("download") != "1" {
			t.Error("query credentials changed")
		}
		if r.URL.EscapedPath() != "/library/parts/1/file%2Fname.mkv" {
			t.Errorf("encoded path changed: %s", r.URL.EscapedPath())
		}
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	proxy := NewProxy(parsed(t, backend.URL), quietLogger())
	defer proxy.Transport.(*http.Transport).CloseIdleConnections()
	server := httptest.NewServer(proxy)
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/library/parts/1/file%2Fname.mkv?X-Plex-Token=query-token&download=1")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
}

func TestProxyFailureDoesNotLogCredentialsOrFilename(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	upstream := parsed(t, backend.URL)
	backend.Close()
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	proxy := NewProxy(upstream, logger)
	defer proxy.Transport.(*http.Transport).CloseIdleConnections()
	request := httptest.NewRequest("GET", "http://public/private-filename.mkv?X-Plex-Token=secret-query-token", nil)
	request.Header.Set("X-Plex-Token", "secret-header-token")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != 502 {
		t.Fatal(response.Code)
	}
	for _, secret := range []string{"private-filename", "secret-query-token", "secret-header-token"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(response.Body.String(), secret) {
			t.Fatal("request details leaked")
		}
	}
}
