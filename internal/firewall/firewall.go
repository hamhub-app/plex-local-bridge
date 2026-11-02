// Package firewall 只管理专属 NAT 链，不清空 DSM 或其它套件的规则。
package firewall

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const Chain = "PLB_GATE_V1"
const owner = "PlexLocalBridge"

type Runner func(context.Context, string, ...string) ([]byte, error)

type Manager struct {
	Run        Runner
	Binaries   []string
	PublicPort int
	ListenPort int
}

func SystemRunner(ctx context.Context, binary string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = append(command.Environ(), "LC_ALL=C")
	return command.CombinedOutput()
}

func (m Manager) command(ctx context.Context, binary string, args ...string) error {
	output, err := m.output(ctx, binary, args...)
	if err != nil {
		return commandFailure(binary, args, output, err)
	}
	return nil
}

// 防火墙命令只包含受控链名和端口；保留有界的系统错误，不能只剩 exit status。
func commandFailure(binary string, args []string, output []byte, err error) error {
	detail := strings.TrimSpace(strings.ToValidUTF8(string(output[:min(len(output), 4096)]), "�"))
	if detail == "" {
		return fmt.Errorf("%s %s failed: %w", binary, strings.Join(args, " "), err)
	}
	return fmt.Errorf("%s %s failed: %w: %s", binary, strings.Join(args, " "), err, detail)
}

func (m Manager) output(ctx context.Context, binary string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	return m.Run(commandCtx, binary, append([]string{"-w", "5", "-t", "nat"}, args...)...)
}

// ownedChain 同名链里存在其它规则时拒绝修改；查表失败不能冒充“已清理”。
func (m Manager) ownedChain(ctx context.Context, binary string) (bool, error) {
	output, err := m.output(ctx, binary, "-S", Chain)
	if err != nil {
		if tableOutput, err := m.output(ctx, binary, "-S"); err != nil {
			// 未启用 IPv6 NAT 的内核没有可清理的表；权限或执行错误仍需明确上报。
			if missingTable(tableOutput) {
				return false, nil
			}
			return false, fmt.Errorf("cannot inspect NAT table: %w", commandFailure(binary, []string{"-S"}, tableOutput, err))
		}
		return false, nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" || line == "-N "+Chain {
			continue
		}
		line = strings.ReplaceAll(line, `"`+owner+`"`, owner)
		if !ownedRedirect(strings.Fields(line)) {
			return true, fmt.Errorf("refusing to modify %s: contains rules not owned by %s", Chain, owner)
		}
	}
	return true, nil
}

func (m Manager) jump() []string { return []string{"-j", Chain} }
func (m Manager) redirect() []string {
	return []string{"-p", "tcp", "--dport", strconv.Itoa(m.PublicPort), "-m", "addrtype", "--dst-type", "LOCAL", "-j", "REDIRECT", "--to-ports", strconv.Itoa(m.ListenPort)}
}

// 群晖可能没有 xt_comment；通过保留链名和完整规则形态识别归属，兼容旧版注释规则。
func ownedRedirect(args []string) bool {
	if len(args) < 2 || args[0] != "-A" || args[1] != Chain {
		return false
	}
	seen := map[string]bool{}
	for i := 2; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return false
		}
		key, value := args[i], args[i+1]
		switch key {
		case "-p":
			if value != "tcp" {
				return false
			}
		case "-m":
			if value != "tcp" && value != "addrtype" && value != "comment" {
				return false
			}
			key += ":" + value
		case "--dport", "--to-ports":
			port, err := strconv.Atoi(value)
			if err != nil || port < 1024 || port > 65535 {
				return false
			}
		case "--dst-type":
			if value != "LOCAL" {
				return false
			}
		case "-j":
			if value != "REDIRECT" {
				return false
			}
		case "--comment":
			if value != owner {
				return false
			}
		default:
			return false
		}
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	return seen["-p"] && seen["--dport"] && seen["-m:addrtype"] && seen["--dst-type"] && seen["-j"] && seen["--to-ports"] && seen["-m:comment"] == seen["--comment"]
}

// Ensure 在代理就绪后接管 PREROUTING；本机 OUTPUT 不改写，因此上游请求不会回到代理。
func (m Manager) Ensure(ctx context.Context) error {
	for _, binary := range m.Binaries {
		exists, err := m.ownedChain(ctx, binary)
		if err != nil {
			return err
		}
		if !exists {
			if err := m.command(ctx, binary, "-N", Chain); err != nil {
				return err
			}
		}
		if err := m.command(ctx, binary, append([]string{"-C", Chain}, m.redirect()...)...); err != nil {
			if err := m.command(ctx, binary, "-F", Chain); err != nil {
				return err
			}
			if err := m.command(ctx, binary, append([]string{"-A", Chain}, m.redirect()...)...); err != nil {
				return err
			}
		}
		if err := m.command(ctx, binary, append([]string{"-C", "PREROUTING"}, m.jump()...)...); err != nil {
			if err := m.command(ctx, binary, append([]string{"-I", "PREROUTING", "1"}, m.jump()...)...); err != nil {
				return err
			}
		}
	}
	return nil
}

// Remove 先断开入口再删除专属链；已清理或部分安装时也能重复调用。
func (m Manager) Remove(ctx context.Context) error {
	var first error
	for _, binary := range m.Binaries {
		exists, ownershipErr := m.ownedChain(ctx, binary)
		if ownershipErr != nil {
			if first == nil {
				first = ownershipErr
			}
		}
		// 同时清理旧版带注释的入口；只对保留链执行有界、可重复的清理。
		for _, jump := range [][]string{m.jump(), {"-m", "comment", "--comment", owner, "-j", Chain}} {
			for i := 0; i < 8; i++ {
				if m.command(ctx, binary, append([]string{"-C", "PREROUTING"}, jump...)...) != nil {
					break
				}
				if err := m.command(ctx, binary, append([]string{"-D", "PREROUTING"}, jump...)...); err != nil {
					if first == nil {
						first = err
					}
					break
				}
			}
		}
		// 即使链被其它服务混入规则，仍撤销自己的入口；保留整个混合链并报告错误。
		if exists && ownershipErr == nil {
			if err := m.command(ctx, binary, "-F", Chain); err != nil && first == nil {
				first = err
			}
			if err := m.command(ctx, binary, "-X", Chain); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}
