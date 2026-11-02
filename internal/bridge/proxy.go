// Package bridge 只在本机 Plex 与客户端之间流式转发，不扫描、缓存或转码媒体。
package bridge

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CheckUpstream 拒绝未认领或允许匿名读取资料库的上游，避免本地代理放大账号权限。
func CheckUpstream(ctx context.Context, upstream *url.URL) error {
	client := &http.Client{
		Timeout:       4 * time.Second,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	u := *upstream
	u.Path = "/identity"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("Accept", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return errors.New("cannot connect to local Plex identity endpoint")
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	response.Body.Close()
	if readErr != nil || len(data) > 64*1024 || response.StatusCode != 200 {
		return errors.New("Plex identity response is invalid")
	}
	var jsonIdentity struct {
		Container struct {
			Claimed any `json:"claimed"`
		} `json:"MediaContainer"`
	}
	var xmlIdentity struct {
		Claimed string `xml:"claimed,attr"`
	}
	claimed := false
	if json.Unmarshal(data, &jsonIdentity) == nil {
		claimed = jsonIdentity.Container.Claimed == true || jsonIdentity.Container.Claimed == float64(1) || jsonIdentity.Container.Claimed == "1"
	} else if xml.Unmarshal(data, &xmlIdentity) == nil {
		claimed = xmlIdentity.Claimed == "1" || xmlIdentity.Claimed == "true"
	}
	if !claimed {
		return errors.New("Plex must be claimed by an account before enabling the bridge")
	}
	u.Path = "/library/sections"
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("Range", "bytes=0-0")
	response, err = client.Do(req)
	if err != nil {
		return errors.New("cannot verify Plex authentication requirements")
	}
	response.Body.Close()
	if response.StatusCode != 401 && response.StatusCode != 403 {
		return fmt.Errorf("Plex must require authentication for the local bridge (anonymous library status %d); clear allowed-without-auth networks", response.StatusCode)
	}
	return nil
}

type bufferPool struct{ pool sync.Pool }

func (p *bufferPool) Get() []byte {
	if b, ok := p.pool.Get().([]byte); ok {
		return b
	}
	return make([]byte, 32*1024)
}
func (p *bufferPool) Put(b []byte) { p.pool.Put(b) }

// NewProxy 保留 Plex 凭据、Range 与响应正文；外部转发头不作为本地连接身份传递。
func NewProxy(upstream *url.URL, logger *slog.Logger) *httputil.ReverseProxy {
	transport := &http.Transport{
		Proxy:        nil,
		DialContext:  (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns: 64, MaxIdleConnsPerHost: 32, MaxConnsPerHost: 256,
		IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression: true,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Host = upstream.Host
			for h := range r.Out.Header {
				if strings.HasPrefix(strings.ToLower(h), "x-forwarded-") {
					r.Out.Header.Del(h)
				}
			}
			for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP", "X-Client-IP", "True-Client-IP"} {
				r.Out.Header.Del(h)
			}
		},
		Transport:     transport,
		BufferPool:    &bufferPool{},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// 不记录 URL、请求头或原始 error，避免 Plex token 进入日志。
			logger.Warn("upstream request failed", "error_type", fmt.Sprintf("%T", err))
			http.Error(w, "Plex is temporarily unavailable.", http.StatusBadGateway)
		},
	}
}
