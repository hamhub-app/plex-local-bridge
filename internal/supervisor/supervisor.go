// Package supervisor 把高权限防火墙操作与低权限网络代理分离。
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hamhub-app/plex-local-bridge/internal/bridge"
	"github.com/hamhub-app/plex-local-bridge/internal/config"
	"github.com/hamhub-app/plex-local-bridge/internal/firewall"
)

func Firewall(c config.Config) (firewall.Manager, error) {
	path, err := exec.LookPath("iptables")
	if err != nil {
		return firewall.Manager{}, errors.New("iptables is required for native Plex port interception")
	}
	binaries := []string{path}
	if c.InterceptIPv6 {
		path, err := exec.LookPath("ip6tables")
		if err != nil {
			return firewall.Manager{}, errors.New("ip6tables unavailable; configure intercept_ipv6=false only if IPv6 interception is not needed")
		}
		binaries = append(binaries, path)
	}
	return firewall.Manager{Run: firewall.SystemRunner, Binaries: binaries, PublicPort: c.PublicPort, ListenPort: c.ListenPort}, nil
}

// RecoveryFirewall 不依赖可用配置；配置损坏或修改地址后也必须能撤销遗留入口。
func RecoveryFirewall() (firewall.Manager, error) {
	c := config.Default()
	c.InterceptIPv6 = false
	m, err := Firewall(c)
	if err != nil {
		return m, err
	}
	if path, err := exec.LookPath("ip6tables"); err == nil {
		m.Binaries = append(m.Binaries, path)
	}
	return m, nil
}

func Run(ctx context.Context, c config.Config, state, username string, logger *slog.Logger) (result error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("supervise requires root on DSM/Linux; use serve for local development")
	}
	account, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("package service account not found: %w", err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil || uid == 0 {
		return errors.New("proxy must run as a non-root package account")
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return err
	}
	c, err = c.Resolve()
	if err != nil {
		return err
	}
	manager, err := Firewall(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(state, 0750); err != nil {
		return err
	}
	if err := os.Chown(state, 0, int(gid)); err != nil {
		return err
	}
	if err := os.Chmod(state, 0750); err != nil {
		return err
	}
	// 同时启动时只允许一个监督进程写 pid 和操作专属规则。
	lock, err := os.OpenFile(filepath.Join(state, "supervisor.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another supervisor holds the service lock")
	}
	pidFile := filepath.Join(state, "supervisor.pid")
	if _, err := RunningPID(state); err == nil {
		return errors.New("supervisor already running")
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		return err
	}
	defer os.Remove(pidFile)
	// 清理上次断电或 SIGKILL 遗留的入口；任何后续启动失败都保持原生 Plex 入口。
	cleanup := func() error {
		os.Remove(filepath.Join(state, "active"))
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		return manager.Remove(cleanupCtx)
	}
	if err := cleanup(); err != nil {
		return err
	}
	defer func() {
		if err := cleanup(); err != nil {
			logger.Error("failed to remove interception rules", "error", err)
			result = errors.Join(result, err)
		}
	}()
	runDir := filepath.Join(state, "proxy")
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return err
	}
	if err := os.Chown(runDir, int(uid), int(gid)); err != nil {
		return err
	}
	runtimeConfig := filepath.Join(state, "runtime.json")
	if err := config.Save(runtimeConfig, c); err != nil {
		return err
	}
	if err := os.Chown(runtimeConfig, 0, int(gid)); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	socket := filepath.Join(runDir, "control.sock")
	child := exec.Command(executable, "serve", "--config", runtimeConfig, "--socket", socket)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	if err := child.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	defer func() {
		// 撤销入口后再停止代理；现有连接有短暂退出窗口。
		if err := cleanup(); err != nil {
			logger.Error("cleanup before proxy stop failed", "error", err)
		}
		child.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			child.Process.Kill()
			<-done
		}
	}()
	readyDeadline := time.Now().Add(15 * time.Second)
	for {
		if bridge.Health(ctx, socket) == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			return errors.New("proxy did not become ready; original Plex port was not intercepted")
		}
		select {
		case err := <-done:
			done <- err
			return errors.New("proxy exited during startup; inspect service.log")
		case <-ctx.Done():
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := manager.Ensure(ctx); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(state, "active"), []byte("ready\n"), 0644); err != nil {
		return err
	}
	logger.Info("native Plex entry intercepted", "public_port", c.PublicPort, "listen_port", c.ListenPort)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			done <- err
			return errors.New("proxy exited; restoring native Plex entry")
		case <-ticker.C:
			if bridge.Health(ctx, socket) != nil {
				return errors.New("proxy unhealthy; restoring native Plex entry")
			}
			// DSM 防火墙重载可能删除 NAT 链；只恢复本套件规则。
			if err := manager.Ensure(ctx); err != nil {
				return err
			}
		}
	}
}

// RunningPID 校验进程身份，不能拿过期 pid 文件去停止其它服务。
func RunningPID(state string) (int, error) {
	b, err := os.ReadFile(filepath.Join(state, "supervisor.pid"))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid < 2 {
		return 0, errors.New("invalid supervisor pid")
	}
	command, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !strings.Contains(string(command), "plex-local-bridge\x00supervise\x00") {
		return 0, errors.New("supervisor is not running")
	}
	return pid, nil
}

func Stop(state string) error {
	pid, err := RunningPID(state)
	if err != nil {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := RunningPID(state); err != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("supervisor did not stop; run recover to remove interception rules")
}
