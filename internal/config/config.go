// Package config 管理套件配置；上游限定为本机 Plex，不能作为开放代理使用。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

type Config struct {
	UpstreamURL    string `json:"upstream_url"`
	PublicPort     int    `json:"public_port"`
	ListenPort     int    `json:"listen_port"`
	InterceptIPv6  bool   `json:"intercept_ipv6"`
	MaxConnections int    `json:"max_connections"`
}

func Default() Config {
	return Config{UpstreamURL: "auto", PublicPort: 32400, ListenPort: 13240, InterceptIPv6: true, MaxConnections: 256}
}

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	c := Default()
	data, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil {
		return c, err
	}
	if len(data) > 64*1024 {
		return c, errors.New("configuration exceeds 64 KiB")
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return c, errors.New("configuration must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, errors.New("configuration must contain one JSON object")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.PublicPort < 1024 || c.PublicPort > 65535 || c.ListenPort < 1024 || c.ListenPort > 65535 {
		return errors.New("ports must be between 1024 and 65535")
	}
	if c.PublicPort == c.ListenPort {
		return errors.New("public_port and listen_port must differ")
	}
	if c.MaxConnections < 1 || c.MaxConnections > 4096 {
		return errors.New("max_connections must be between 1 and 4096")
	}
	if c.UpstreamURL == "auto" {
		if c.ListenPort == 32400 {
			return errors.New("listen_port conflicts with the native Plex port used by auto")
		}
		return nil
	}
	u, err := url.Parse(c.UpstreamURL)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("upstream_url must be auto or a local http://IP:port without credentials, path or query")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("upstream_url requires an explicit port")
	}
	if port == c.ListenPort {
		return errors.New("upstream must not point at the proxy listener")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		return errors.New("upstream_url requires a literal local IP address")
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	for _, address := range addresses {
		if a, _, err := net.ParseCIDR(address.String()); err == nil && a.Equal(ip) {
			return nil
		}
	}
	return errors.New("upstream IP must belong to this NAS")
}

// Resolve 选择 NAS 的私有 IPv4 地址，避免把 loopback 一概当成 Plex 本地播放。
func (c Config) Resolve() (Config, error) {
	if err := c.Validate(); err != nil {
		return c, err
	}
	if c.UpstreamURL != "auto" {
		return c, nil
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return c, err
	}
	// DSM 的默认路由可能在 bond/OVS 接口上；优先该接口，避免误选 Docker 网桥。
	routes, _ := os.ReadFile("/proc/net/route")
	preferred := defaultInterface(string(routes))
	sort.SliceStable(interfaces, func(i, j int) bool { return interfaces[i].Name == preferred && interfaces[j].Name != preferred })
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && ip.IsPrivate() {
				c.UpstreamURL = "http://" + net.JoinHostPort(ip.String(), "32400")
				return c, nil
			}
		}
	}
	return c, errors.New("no private IPv4 address found; configure upstream_url with the NAS LAN IP")
}

func defaultInterface(routes string) string {
	for _, line := range strings.Split(routes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err == nil && flags&1 != 0 {
			return fields[0]
		}
	}
	return ""
}

func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0640)
}
