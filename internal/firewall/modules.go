package firewall

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var moduleName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// ModuleLoader 仅按现有模块元数据解析依赖，不下载、编译或卸载内核模块。
type ModuleLoader struct {
	Loaded       func(string) bool
	Dependencies func(string) ([]string, error)
	Insert       func(context.Context, string) error
}

func (l ModuleLoader) Load(ctx context.Context, names ...string) error {
	visiting, done := map[string]bool{}, map[string]bool{}
	var load func(string, int) error
	load = func(name string, depth int) error {
		if !moduleName.MatchString(name) {
			return fmt.Errorf("invalid kernel module name %q", name)
		}
		if done[name] || l.Loaded(name) {
			done[name] = true
			return nil
		}
		if depth > 24 || len(done)+len(visiting) > 128 || visiting[name] {
			return errors.New("kernel module dependency cycle or limit exceeded")
		}
		visiting[name] = true
		deps, err := l.Dependencies(name)
		if err != nil {
			return err
		}
		for _, dep := range deps {
			if err := load(dep, depth+1); err != nil {
				return err
			}
		}
		if err := l.Insert(ctx, name); err != nil && !l.Loaded(name) {
			return err
		}
		delete(visiting, name)
		done[name] = true
		return nil
	}
	for _, name := range names {
		if err := load(name, 0); err != nil {
			return err
		}
	}
	return nil
}

func moduleMetadata(data []byte, kernel string) ([]string, error) {
	var version string
	var deps []string
	for _, field := range strings.Split(string(data), "\x00") {
		if strings.HasPrefix(field, "vermagic=") {
			values := strings.Fields(strings.TrimPrefix(field, "vermagic="))
			if len(values) > 0 {
				version = values[0]
			}
		}
		if strings.HasPrefix(field, "depends=") {
			for _, dep := range strings.Split(strings.TrimPrefix(field, "depends="), ",") {
				if dep != "" {
					deps = append(deps, strings.ReplaceAll(dep, "-", "_"))
				}
			}
		}
	}
	if version == "" || version != kernel {
		return nil, fmt.Errorf("kernel module version %q does not match running kernel %q", version, kernel)
	}
	return deps, nil
}

func systemModules() (ModuleLoader, error) {
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ModuleLoader{}, err
	}
	insmod, err := exec.LookPath("insmod")
	if err != nil {
		return ModuleLoader{}, errors.New("insmod is required to load existing DSM netfilter modules")
	}
	return ModuleLoader{
		Loaded: func(name string) bool {
			data, _ := os.ReadFile("/proc/modules")
			for _, line := range strings.Split(string(data), "\n") {
				f := strings.Fields(line)
				if len(f) > 0 && f[0] == name {
					return true
				}
			}
			return false
		},
		Dependencies: func(name string) ([]string, error) {
			path := filepath.Join("/lib/modules", name+".ko")
			stat, err := os.Lstat(path)
			if err != nil {
				return nil, fmt.Errorf("required DSM module %s unavailable: %w", name, err)
			}
			owner, ok := stat.Sys().(*syscall.Stat_t)
			if !ok || owner.Uid != 0 || !stat.Mode().IsRegular() || stat.Mode().Perm()&0022 != 0 {
				return nil, fmt.Errorf("refusing untrusted kernel module %s", path)
			}
			file, err := elf.Open(path)
			if err != nil {
				return nil, fmt.Errorf("invalid kernel module %s: %w", name, err)
			}
			defer file.Close()
			section := file.Section(".modinfo")
			if section == nil || section.Size > 65536 {
				return nil, fmt.Errorf("invalid module metadata for %s", name)
			}
			data, err := section.Data()
			if err != nil {
				return nil, err
			}
			return moduleMetadata(data, strings.TrimSpace(string(kernel)))
		},
		Insert: func(ctx context.Context, name string) error {
			loadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			args := []string{filepath.Join("/lib/modules", name+".ko")}
			out, err := exec.CommandContext(loadCtx, insmod, args...).CombinedOutput()
			if err != nil {
				return commandFailure(insmod, args, out, err)
			}
			return nil
		},
	}, nil
}

// PrepareDSM 仅由管理员启用步骤调用；针对 DSM 平铺模块目录补齐 legacy NAT 的显式依赖。
func (m Manager) PrepareDSM(ctx context.Context) error {
	for _, binary := range m.Binaries {
		version, versionErr := m.Run(ctx, binary, "--version")
		if versionErr != nil {
			return commandFailure(binary, []string{"--version"}, version, versionErr)
		}
		out, probeErr := m.output(ctx, binary, "-S")
		if probeErr != nil && !missingTable(out) {
			return commandFailure(binary, []string{"-S"}, out, probeErr)
		}
		base := "iptable_nat"
		if strings.Contains(filepath.Base(binary), "ip6tables") {
			base = "ip6table_nat"
		}
		_, flatModuleErr := os.Stat(filepath.Join("/lib/modules", base+".ko"))
		if strings.Contains(string(version), "legacy") && (probeErr != nil || flatModuleErr == nil) {
			loader, err := systemModules()
			if err != nil {
				return err
			}
			if err := loader.Load(ctx, base, "xt_tcpudp", "xt_addrtype", "xt_REDIRECT"); err != nil {
				return err
			}
		} else if probeErr != nil {
			return commandFailure(binary, []string{"-S"}, out, probeErr)
		}
		if out, err := m.output(ctx, binary, "-S"); err != nil {
			return commandFailure(binary, []string{"-S"}, out, err)
		}
	}
	return nil
}

func missingTable(output []byte) bool {
	text := strings.ToLower(string(output))
	return strings.Contains(text, "table does not exist") || strings.Contains(text, "iptables who?") || strings.Contains(text, "protocol not supported")
}
