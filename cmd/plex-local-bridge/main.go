package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hamhub-app/plex-local-bridge/internal/bridge"
	"github.com/hamhub-app/plex-local-bridge/internal/config"
	"github.com/hamhub-app/plex-local-bridge/internal/gatewaystate"
	"github.com/hamhub-app/plex-local-bridge/internal/supervisor"
	"github.com/hamhub-app/plex-local-bridge/internal/webui"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	args := os.Args[1:]
	// CGI 独立入口始终只处理管理请求，不允许 query string 改选命令行子命令。
	if filepath.Base(os.Args[0]) == "api.cgi" {
		args = []string{"webui"}
	}
	if err := run(args, logger); err != nil {
		logger.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string, logger *slog.Logger) error {
	if len(args) == 0 {
		return errors.New("commands: serve, daemon, daemon-status, daemon-stop, gateway, gateway-status, gateway-stop, supervise, init-config, check, status, stop, recover, version")
	}
	command := args[0]
	if command == "webui" {
		return webui.ServeCGI(version)
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	path := f.String("config", "config.json", "configuration file")
	defaultState := "/var/packages/PlexLocalBridge/var/run"
	if strings.HasPrefix(command, "gateway") {
		defaultState = "/var/run/plex-local-bridge"
	}
	state := f.String("state", defaultState, "process state directory")
	socket := f.String("socket", "", "local control socket")
	username := f.String("user", "PlexLocalBridge", "unprivileged DSM package account")
	upstream := f.String("upstream", "auto", "local Plex URL or auto")
	publicPort := f.Int("public-port", 32400, "existing NAS entry port")
	listenPort := f.Int("listen-port", 13240, "proxy listener port")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if command == "init-webui" {
		return webui.Initialize(filepath.Dir(*state))
	}
	if *socket == "" {
		*socket = filepath.Join(*state, "control.sock")
		if command == "serve" {
			*socket = filepath.Join(os.TempDir(), "plex-local-bridge.sock")
		}
		if command == "gateway" {
			*socket = "/var/packages/PlexLocalBridge/var/run/control.sock"
		}
	}
	if command == "daemon-stop" {
		return supervisor.StopCommand(*state, "daemon.pid", "daemon")
	}
	if command == "daemon-status" {
		if _, err := supervisor.RunningCommand(*state, "daemon.pid", "daemon"); err != nil {
			return err
		}
		return bridge.Health(ctx, *socket)
	}
	if command == "gateway-stop" || command == "gateway-status" || command == "gateway-running" {
		if os.Geteuid() != 0 {
			return errors.New("gateway management requires administrator privileges")
		}
		if command == "gateway-stop" {
			return supervisor.StopCommand(*state, "gateway.pid", "gateway")
		}
		if _, err := supervisor.RunningCommand(*state, "gateway.pid", "gateway"); err != nil {
			return err
		}
		if command == "gateway-running" {
			return nil
		}
		if _, err := os.Stat(filepath.Join(*state, "active")); err != nil {
			return errors.New("gateway is waiting for the package proxy")
		}
		return nil
	}
	if command == "init-config" {
		c := config.Default()
		c.UpstreamURL, c.PublicPort, c.ListenPort = *upstream, *publicPort, *listenPort
		return config.Save(*path, c)
	}
	if command == "stop" {
		return supervisor.Stop(*state)
	}
	if command == "status" {
		if _, err := supervisor.RunningPID(*state); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(*state, "active")); err != nil {
			return errors.New("port interception is not active")
		}
		return bridge.Health(ctx, filepath.Join(*state, "proxy", "control.sock"))
	}
	if command == "recover" {
		if os.Geteuid() != 0 {
			return errors.New("recover requires administrator privileges")
		}
		if _, err := supervisor.RunningPID(*state); err == nil {
			return errors.New("stop the running supervisor before recovery")
		}
		if _, err := supervisor.RunningCommand(*state, "gateway.pid", "gateway"); err == nil {
			return errors.New("stop the running gateway before recovery")
		}
		manager, err := supervisor.RecoveryFirewall()
		if err != nil {
			return err
		}
		if err := manager.Remove(ctx); err != nil {
			return err
		}
		return gatewaystate.MarkStopped(*state)
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	if command == "prepare-firewall" {
		if os.Geteuid() != 0 {
			return errors.New("preparing DSM netfilter requires administrator privileges")
		}
		manager, err := supervisor.Firewall(c)
		if err != nil {
			return err
		}
		return manager.PrepareDSM(ctx)
	}
	switch command {
	case "gateway":
		return supervisor.RunGateway(ctx, c, *state, *socket, logger)
	case "supervise":
		return supervisor.Run(ctx, c, *state, *username, logger)
	case "serve", "daemon", "check":
		c, err = c.Resolve()
		if err != nil {
			return err
		}
		if command == "check" {
			u, _ := url.Parse(c.UpstreamURL)
			return bridge.CheckUpstream(ctx, u)
		}
		if command == "daemon" {
			return supervisor.RunProxy(ctx, c, *state, *socket, logger)
		}
		return (&bridge.Server{Config: c, Socket: *socket, Logger: logger}).Run(ctx)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}
