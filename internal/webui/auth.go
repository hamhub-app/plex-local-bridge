package webui

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type apiError struct {
	status  int
	message string
}

// DSM 的 CGI 启动身份可能与套件控制脚本不同；处理会话和配置前先不可逆地降权。
func dropCGIPrivileges() error {
	account, err := user.Lookup("PlexLocalBridge")
	if err != nil {
		return errors.New("无法查找 DSM 套件账号 PlexLocalBridge")
	}
	uid, uidErr := strconv.Atoi(account.Uid)
	gid, gidErr := strconv.Atoi(account.Gid)
	if uidErr != nil || gidErr != nil || uid <= 0 || gid <= 0 {
		return errors.New("DSM 套件账号的 UID/GID 无效")
	}
	if os.Geteuid() == 0 {
		if err := syscall.Setgroups([]int{gid}); err != nil {
			return fmtPrivilegeError("附加组", err)
		}
		if err := syscall.Setgid(gid); err != nil {
			return fmtPrivilegeError("GID", err)
		}
		if err := syscall.Setuid(uid); err != nil {
			return fmtPrivilegeError("UID", err)
		}
	}
	if os.Geteuid() != uid || os.Getegid() != gid {
		return errors.New("DSM CGI 未以 PlexLocalBridge 套件账号运行，已拒绝管理请求")
	}
	return nil
}

func fmtPrivilegeError(step string, err error) error {
	return errors.New("DSM CGI 无法切换至普通套件账号（" + step + "）：" + err.Error())
}

func (e apiError) Error() string { return e.message }

// DSMAuthenticate 使用 DSM 的现有会话，不接收或保存管理员密码。
func DSMAuthenticate(r *http.Request) (string, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/syno/synoman/webman/modules/authenticate.cgi")
	// CGI 环境承载 DSM cookie；丢弃动态加载器变量，避免改变系统校验工具行为。
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "LD_") || key == "GCONV_PATH" || key == "LOCPATH" || key == "NLSPATH" {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return "", apiError{503, "DSM 身份校验超时，请稍后重试"}
	}
	name, err := dsmIdentity(output, err)
	if err != nil {
		return "", err
	}
	groups, err := os.ReadFile("/etc/group")
	if err != nil || !isAdministrator(string(groups), name) {
		return "", apiError{403, "仅 DSM 管理员可以管理 Plex Local Bridge"}
	}
	return name, nil
}

// authenticate.cgi 的接口约定以 stdout 用户名判断会话，未约定成功退出码为 0。
func dsmIdentity(output []byte, commandErr error) (string, error) {
	var exitErr *exec.ExitError
	if commandErr != nil && (!errors.As(commandErr, &exitErr) || exitErr.ExitCode() < 0) {
		return "", apiError{503, "DSM 身份校验工具无法执行：" + commandErr.Error()}
	}
	name := strings.TrimSpace(string(output))
	if name == "" {
		return "", apiError{401, "请先登录 DSM，再从套件中心打开管理界面"}
	}
	if len(name) > 256 || strings.ContainsAny(name, "\r\n\x00") {
		return "", apiError{401, "DSM 会话无效"}
	}
	return name, nil
}

func isAdministrator(groups, name string) bool {
	for _, line := range strings.Split(groups, "\n") {
		fields := strings.SplitN(line, ":", 4)
		if len(fields) != 4 || fields[0] != "administrators" {
			continue
		}
		for _, member := range strings.Split(fields[3], ",") {
			if strings.EqualFold(member, name) {
				return true
			}
		}
	}
	return false
}

func sessionKey(directory string) ([]byte, error) {
	path := filepath.Join(directory, "webui.key")
	if data, err := os.ReadFile(path); err == nil {
		if len(data) != 32 {
			return nil, errors.New("invalid web UI signing key")
		}
		return data, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		return os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	if _, err = f.Write(key); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}

func Initialize(directory string) error { _, err := sessionKey(directory); return err }

func csrfToken(key []byte, user, cookie string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(user))
	h.Write([]byte{0})
	h.Write([]byte(cookie))
	return hex.EncodeToString(h.Sum(nil))
}
