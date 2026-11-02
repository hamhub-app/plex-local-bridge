package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hamhub-app/plex-local-bridge/internal/config"
)

// Server 将明文 HTTP 交给本地反代，TLS 原样透传给 Plex，保持其证书与协议。
type Server struct {
	Config config.Config
	Socket string
	Logger *slog.Logger
}

type channelListener struct {
	address     net.Addr
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}

func (l *channelListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *channelListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *channelListener) Addr() net.Addr { return l.address }

type trackedConn struct {
	net.Conn
	once      sync.Once
	closeHook func()
}

func (c *trackedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.closeHook); return err }

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	upstream, err := url.Parse(s.Config.UpstreamURL)
	if err != nil {
		return err
	}
	if err := CheckUpstream(ctx, upstream); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(s.Config.ListenPort)))
	if err != nil {
		return err
	}
	defer listener.Close()
	// HTTP 和 TLS 共用有界连接数；所有连接随服务退出清理，媒体从不整体读入内存。
	httpListener := &channelListener{address: listener.Addr(), connections: make(chan net.Conn), done: make(chan struct{})}
	defer httpListener.Close()
	proxy := NewProxy(upstream, s.Logger)
	defer proxy.Transport.(*http.Transport).CloseIdleConnections()
	var healthy atomic.Bool
	healthy.Store(true)
	server := &http.Server{
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 * 1024,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !healthy.Load() {
				http.Error(w, "Plex authentication check failed.", 503)
				return
			}
			proxy.ServeHTTP(w, r)
		}),
	}
	var connections sync.Map
	defer func() {
		cancel()
		listener.Close()
		connections.Range(func(key, _ any) bool { key.(net.Conn).Close(); return true })
	}()
	semaphore := make(chan struct{}, s.Config.MaxConnections)
	errorsCh := make(chan error, 3)
	go func() { errorsCh <- server.Serve(httpListener) }()
	defer server.Close()

	if info, err := os.Lstat(s.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("control socket path already contains a non-socket file")
		}
		connection, err := net.DialTimeout("unix", s.Socket, time.Second)
		if err == nil {
			connection.Close()
			return errors.New("control socket is already in use")
		}
		if err := os.Remove(s.Socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	control, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	defer control.Close()
	defer os.Remove(s.Socket)
	if err := os.Chmod(s.Socket, 0600); err != nil {
		return err
	}
	controlServer := &http.Server{ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !healthy.Load() {
			w.WriteHeader(503)
		}
		json.NewEncoder(w).Encode(map[string]any{"healthy": healthy.Load(), "upstream": upstream.String(), "listen_port": s.Config.ListenPort})
	})}
	go func() { errorsCh <- controlServer.Serve(control) }()
	defer controlServer.Close()
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := CheckUpstream(ctx, upstream); err != nil {
					healthy.Store(false)
					s.Logger.Error("Plex authentication verification failed; bridge blocked")
					return
				}
			}
		}
	}()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				errorsCh <- err
				return
			}
			select {
			case semaphore <- struct{}{}:
			default:
				connection.Close()
				continue
			}
			c := &trackedConn{Conn: connection}
			c.closeHook = func() { connections.Delete(c); <-semaphore }
			connections.Store(c, true)
			if ctx.Err() != nil {
				c.Close()
				return
			}
			go func() {
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReaderSize(c, 4096)
				prefix, err := reader.Peek(1)
				if err != nil {
					c.Close()
					return
				}
				c.SetReadDeadline(time.Time{})
				buffered := &bufferedConn{Conn: c, reader: reader}
				if prefix[0] == 0x16 {
					if healthy.Load() {
						s.tunnel(ctx, buffered, upstream.Host)
					} else {
						c.Close()
					}
					return
				}
				select {
				case httpListener.connections <- buffered:
				case <-ctx.Done():
					c.Close()
				case <-httpListener.done:
					c.Close()
				}
			}()
		}
	}()
	s.Logger.Info("proxy ready", "listen_port", s.Config.ListenPort, "tls", "passthrough")
	select {
	case <-ctx.Done():
		return nil
	case err := <-errorsCh:
		return err
	}
}

// tunnel 不终止 TLS，也不能修改其 HTTP Host/转发头；此路径不保证改变 Plex 远程判定。
func (s *Server) tunnel(ctx context.Context, client net.Conn, address string) {
	defer client.Close()
	upstream, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return
	}
	defer upstream.Close()
	done := make(chan struct{})
	go func() { io.Copy(upstream, client); upstream.Close(); close(done) }()
	io.Copy(client, upstream)
	client.Close()
	<-done
}

func Health(ctx context.Context, socket string) error {
	return health(ctx, socket, nil)
}

// HealthMatches 保证管理员批准的端口、上游与普通代理一致，配置变更后先撤销接管。
func HealthMatches(ctx context.Context, socket string, expected config.Config) error {
	return health(ctx, socket, &expected)
}

func health(ctx context.Context, socket string, expected *config.Config) error {
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, DisableKeepAlives: true}}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost/health", nil)
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("proxy is not healthy")
	}
	var status struct {
		Healthy    bool   `json:"healthy"`
		Upstream   string `json:"upstream"`
		ListenPort int    `json:"listen_port"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16*1024)).Decode(&status); err != nil {
		return errors.New("invalid proxy health response")
	}
	if !status.Healthy {
		return errors.New("proxy is not healthy")
	}
	if expected != nil && (status.ListenPort != expected.ListenPort || status.Upstream != expected.UpstreamURL) {
		return errors.New("proxy configuration differs from administrator-approved gateway configuration")
	}
	return nil
}
