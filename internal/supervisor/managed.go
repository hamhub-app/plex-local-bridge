package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hamhub-app/plex-local-bridge/internal/bridge"
	"github.com/hamhub-app/plex-local-bridge/internal/config"
	"github.com/hamhub-app/plex-local-bridge/internal/gatewaystate"
)

// RunningCommand 同时核对程序名和子命令，避免过期 PID 指向其它进程。
func RunningCommand(state, filename, role string) (int, error) {
	data, err := os.ReadFile(filepath.Join(state, filename))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 2 {
		return 0, errors.New("invalid process pid")
	}
	data, err = os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	args := strings.Split(string(data), "\x00")
	if err != nil || len(args) < 2 || filepath.Base(args[0]) != "plex-local-bridge" || args[1] != role {
		return 0, errors.New("managed process is not running")
	}
	return pid, nil
}

func StopCommand(state, filename, role string) error {
	pid, err := RunningCommand(state, filename, role)
	if err != nil {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := RunningCommand(state, filename, role); err != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("managed process did not stop")
}

func claimProcess(state, role string) (func(), error) {
	if err := os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(state, role+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("managed process already running")
	}
	pidFile := filepath.Join(state, role+".pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		lock.Close()
		return nil, err
	}
	return func() { os.Remove(pidFile); lock.Close() }, nil
}

// RunProxy 是 DSM 普通套件入口，不申请 root，也不操作防火墙。
func RunProxy(ctx context.Context, c config.Config, state, socket string, logger *slog.Logger) error {
	if os.Geteuid() == 0 {
		return errors.New("daemon must run as the unprivileged package account")
	}
	release, err := claimProcess(state, "daemon")
	if err != nil {
		return err
	}
	defer release()
	return (&bridge.Server{Config: c, Socket: socket, Logger: logger}).Run(ctx)
}

// RunGateway 只由管理员显式启动；监督已经运行的普通代理，不以 root 执行套件内程序。
func RunGateway(ctx context.Context, c config.Config, state, socket string, logger *slog.Logger) (result error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("gateway requires root on DSM/Linux")
	}
	c, err := c.Resolve()
	if err != nil {
		return err
	}
	manager, err := Firewall(c)
	if err != nil {
		return err
	}
	recovery, err := RecoveryFirewall()
	if err != nil {
		return err
	}
	release, err := claimProcess(state, "gateway")
	if err != nil {
		return err
	}
	defer release()
	group, err := user.LookupGroup("PlexLocalBridge")
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return err
	}
	if err := os.Chown(state, 0, gid); err != nil {
		return err
	}
	if err := os.Chmod(state, 0750); err != nil {
		return err
	}
	runID := strconv.FormatInt(time.Now().UnixNano(), 10)
	publish := func(phase, message string) error {
		return gatewaystate.Write(state, gid, gatewaystate.Snapshot{RunID: runID, Phase: phase, Message: message, Config: c})
	}
	defer func() {
		phase, message := "stopped", "管理员端口接管已停止"
		if result != nil {
			phase = "failed"
			message = result.Error()
		}
		if err := publish(phase, message); err != nil {
			logger.Error("cannot publish gateway status", "error", err)
		}
	}()
	cleanup := func() error {
		os.Remove(filepath.Join(state, "active"))
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		return recovery.Remove(cleanupCtx)
	}
	if err := cleanup(); err != nil {
		return err
	}
	defer func() { result = errors.Join(result, cleanup()) }()
	if err := publish("preparing", "检查 NAT 支持及系统内核模块"); err != nil {
		return err
	}
	if err := manager.PrepareDSM(ctx); err != nil {
		return err
	}
	active := false
	logger.Info("gateway waiting for the package proxy")
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if bridge.HealthMatches(ctx, socket, c) == nil {
			if !active {
				upstream, parseErr := url.Parse(c.UpstreamURL)
				if parseErr != nil {
					return parseErr
				}
				if err := bridge.CheckUpstream(ctx, upstream); err != nil {
					return err
				}
			}
			if err := manager.Ensure(ctx); err != nil {
				return err
			}
			if !active {
				if err := os.WriteFile(filepath.Join(state, "active"), []byte("ready\n"), 0600); err != nil {
					return err
				}
				logger.Info("native Plex entry intercepted", "public_port", c.PublicPort, "listen_port", c.ListenPort)
				active = true
			}
			if err := publish("active", ""); err != nil {
				return err
			}
		} else if active {
			if err := cleanup(); err != nil {
				return err
			}
			active = false
			logger.Info("package proxy unavailable; native Plex entry restored")
		}
		if !active {
			if err := publish("waiting", "等待普通代理启动，或等待配置与管理员批准副本一致"); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
