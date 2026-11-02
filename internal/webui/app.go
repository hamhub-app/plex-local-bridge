// Package webui 通过 DSM 的 CGI 提供管理 API，不在媒体端口暴露管理接口。
package webui

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hamhub-app/plex-local-bridge/internal/bridge"
	"github.com/hamhub-app/plex-local-bridge/internal/config"
	"github.com/hamhub-app/plex-local-bridge/internal/gatewaystate"
	"github.com/hamhub-app/plex-local-bridge/internal/supervisor"
)

const packageBase = "/var/packages/PlexLocalBridge"

type App struct {
	Version                                     string
	ConfigPath, VarPath, StatePath, GatewayPath string
	Authenticate                                func(*http.Request) (string, error)
	Control                                     func(context.Context, string) error
	Probe                                       func(context.Context, config.Config) error
	Running                                     func() bool
}

type State struct {
	Version        string                `json:"version"`
	User           string                `json:"user"`
	Config         config.Config         `json:"config"`
	Revision       string                `json:"revision"`
	ConfigError    string                `json:"config_error,omitempty"`
	CSRF           string                `json:"csrf"`
	ProxyRunning   bool                  `json:"proxy_running"`
	ProxyError     string                `json:"proxy_error,omitempty"`
	Gateway        gatewaystate.Snapshot `json:"gateway"`
	GatewayError   string                `json:"gateway_error,omitempty"`
	ApprovalNeeded bool                  `json:"approval_needed"`
	RecentErrors   []string              `json:"recent_errors"`
}

func Installed(version string) *App {
	return &App{
		Version: version, ConfigPath: packageBase + "/etc/config.json", VarPath: packageBase + "/var", StatePath: packageBase + "/var/run", GatewayPath: "/var/run/plex-local-bridge", Authenticate: DSMAuthenticate,
		Control: func(ctx context.Context, action string) error {
			commandCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
			defer cancel()
			output, err := exec.CommandContext(commandCtx, packageBase+"/scripts/start-stop-status", action).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %w: %s", action, err, strings.TrimSpace(string(output[:min(len(output), 4096)])))
			}
			return nil
		},
		Probe: func(ctx context.Context, c config.Config) error {
			resolved, err := c.Resolve()
			if err != nil {
				return err
			}
			upstream, err := url.Parse(resolved.UpstreamURL)
			if err != nil {
				return err
			}
			return bridge.CheckUpstream(ctx, upstream)
		},
	}
}

func ServeCGI(version string) error {
	if os.Getenv("GATEWAY_INTERFACE") == "" {
		return startupFailure(errors.New("未检测到 DSM CGI 请求环境"))
	}
	if err := dropCGIPrivileges(); err != nil {
		return startupFailure(err)
	}
	if err := cgi.Serve(Installed(version)); err != nil {
		return startupFailure(fmt.Errorf("DSM CGI 请求环境无效：%w", err))
	}
	return nil
}

// 启动失败也返回 CGI 响应，避免 DSM 用无上下文的 503 HTML 掩盖实际原因。
func startupFailure(err error) error {
	fmt.Print("Status: 200 OK\r\nContent-Type: application/json; charset=utf-8\r\nCache-Control: no-store\r\n\r\n")
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"error": err.Error(), "code": 503})
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	user, err := a.Authenticate(r)
	if err != nil {
		a.fail(w, err)
		return
	}
	key, err := sessionKey(a.VarPath)
	if err != nil {
		a.fail(w, apiError{503, "无法读取管理会话保护信息"})
		return
	}
	token := csrfToken(key, user, r.Header.Get("Cookie"))
	action := r.URL.Query().Get("action")
	if r.Method == "GET" && (action == "status" || action == "") {
		json.NewEncoder(w).Encode(a.state(r.Context(), user, token))
		return
	}
	if r.Method != "POST" {
		a.fail(w, apiError{405, "不支持此操作"})
		return
	}
	if !hmac.Equal([]byte(token), []byte(r.Header.Get("X-PLB-CSRF"))) {
		a.fail(w, apiError{403, "管理会话已变化，请刷新页面后重试"})
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
			a.fail(w, apiError{403, "拒绝跨站管理请求"})
			return
		}
	}
	if action != "save" {
		a.fail(w, apiError{404, "未知管理操作"})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		a.fail(w, apiError{415, "配置必须使用 JSON 格式"})
		return
	}
	var body struct {
		Config   config.Config `json:"config"`
		Revision string        `json:"revision"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		a.fail(w, apiError{400, "配置内容无效"})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		a.fail(w, apiError{400, "只允许提交一份配置"})
		return
	}
	if err := body.Config.Validate(); err != nil {
		a.fail(w, apiError{400, err.Error()})
		return
	}
	if err := a.apply(r.Context(), body.Config, body.Revision); err != nil {
		a.fail(w, err)
		return
	}
	json.NewEncoder(w).Encode(a.state(r.Context(), user, token))
}

func (a *App) fail(w http.ResponseWriter, err error) {
	status := 500
	var api apiError
	if errors.As(err, &api) {
		status = api.status
	}
	// DSM nginx 会将 5xx 正文替换成统一 HTML；用 JSON 错误码保留服务端诊断。
	if status >= 500 {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(status)
	}
	json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "code": status})
}

func revision(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }

func (a *App) state(ctx context.Context, user, token string) State {
	data, _ := configurationData(a.ConfigPath)
	c, err := config.Load(a.ConfigPath)
	s := State{Version: a.Version, User: user, Config: c, Revision: revision(data), CSRF: token}
	if err != nil {
		s.Config = config.Default()
		s.ConfigError = err.Error()
	}
	if _, err := supervisor.RunningCommand(a.StatePath, "daemon.pid", "daemon"); err == nil {
		if err := bridge.Health(ctx, filepath.Join(a.StatePath, "control.sock")); err == nil {
			s.ProxyRunning = true
		} else {
			s.ProxyError = err.Error()
		}
	} else {
		s.ProxyError = "代理未运行"
	}
	s.Gateway, err = gatewaystate.Read(a.GatewayPath)
	if err != nil {
		s.Gateway.Phase = "unavailable"
		s.GatewayError = "尚未读取到管理员接管状态；若已启用旧版接管，请更新管理员副本"
	}
	if err == nil {
		expected, resolveErr := s.Config.Resolve()
		approved := s.Gateway.Config
		s.ApprovalNeeded = resolveErr != nil || expected.UpstreamURL != approved.UpstreamURL || expected.PublicPort != approved.PublicPort || expected.ListenPort != approved.ListenPort || expected.InterceptIPv6 != approved.InterceptIPv6
	} else {
		s.ApprovalNeeded = true
	}
	s.RecentErrors = recentErrors(filepath.Join(a.VarPath, "service.log"))
	return s
}

func (a *App) apply(ctx context.Context, c config.Config, expectedRevision string) error {
	lock, err := os.OpenFile(filepath.Join(a.VarPath, "webui.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return apiError{409, "另一个配置操作正在执行，请稍后重试"}
	}
	old, err := configurationData(a.ConfigPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if revision(old) != expectedRevision {
		return apiError{409, "配置已被其它窗口修改，请刷新后再保存"}
	}
	if err := a.Probe(ctx, c); err != nil {
		return apiError{400, "Plex 上游检查失败：" + err.Error()}
	}
	previous, previousErr := config.Load(a.ConfigPath)
	if previousErr != nil || c.ListenPort != previous.ListenPort {
		listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(c.ListenPort)))
		if err != nil {
			return apiError{409, "代理监听端口不可用：" + err.Error()}
		}
		listener.Close()
	}
	_, runningErr := supervisor.RunningCommand(a.StatePath, "daemon.pid", "daemon")
	wasRunning := runningErr == nil
	if a.Running != nil {
		wasRunning = a.Running()
	}
	if err := a.Control(ctx, "stop"); err != nil {
		return fmt.Errorf("停止代理失败，配置未修改：%w", err)
	}
	encoded, _ := json.MarshalIndent(c, "", "  ")
	if err := atomicWrite(a.ConfigPath, append(encoded, '\n')); err != nil {
		if wasRunning {
			a.Control(context.WithoutCancel(ctx), "start")
		}
		return err
	}
	if err := a.Control(ctx, "start"); err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
		a.Control(rollbackCtx, "stop")
		rollbackErr := atomicWrite(a.ConfigPath, old)
		if wasRunning && rollbackErr == nil {
			rollbackErr = a.Control(rollbackCtx, "start")
		}
		if rollbackErr != nil {
			return fmt.Errorf("新配置启动失败：%v；恢复原配置失败：%w", err, rollbackErr)
		}
		return apiError{500, "新配置启动失败，已恢复原配置：" + err.Error()}
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".config-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func configurationData(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return nil, err
	}
	if len(data) > 65536 {
		return nil, errors.New("configuration exceeds 64 KiB")
	}
	return data, nil
}

func recentErrors(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	if info.Size() > 16384 {
		f.Seek(info.Size()-16384, io.SeekStart)
	}
	data, _ := io.ReadAll(io.LimitReader(f, 16384))
	var messages []string
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var entry struct{ Level, Msg, Error, Time string }
		if json.Unmarshal(line, &entry) == nil && entry.Level == "ERROR" {
			messages = append(messages, entry.Time+" · "+entry.Msg+" · "+entry.Error)
		}
	}
	if len(messages) > 5 {
		messages = messages[len(messages)-5:]
	}
	return messages
}
