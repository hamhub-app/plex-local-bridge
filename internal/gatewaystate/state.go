// Package gatewaystate 发布普通套件可读、仅管理员进程可写的端口接管状态。
package gatewaystate

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hamhub-app/plex-local-bridge/internal/config"
)

type Snapshot struct {
	RunID     string        `json:"run_id,omitempty"`
	Phase     string        `json:"phase"`
	Message   string        `json:"message,omitempty"`
	UpdatedAt time.Time     `json:"updated_at"`
	Config    config.Config `json:"config"`
}

// MarkStopped 在显式恢复规则后同步结果；旧版尚无状态文件时不创建额外状态。
func MarkStopped(state string) error {
	s, err := Read(state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	group, err := user.LookupGroup("PlexLocalBridge")
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return err
	}
	s.Phase, s.Message = "stopped", "管理员端口接管已停止"
	return Write(state, gid, s)
}

// Write 目标目录必须由 root 持有且不可被套件账号写入，避免跨权限路径替换。
func Write(state string, gid int, s Snapshot) error {
	s.UpdatedAt = time.Now().UTC()
	if len(s.Message) > 8192 {
		s.Message = s.Message[:8192]
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(state, ".gateway-status-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0640); err != nil {
		return err
	}
	if err := f.Chown(0, gid); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(state, "status.json"))
}

func Read(state string) (Snapshot, error) {
	f, err := os.Open(filepath.Join(state, "status.json"))
	if err != nil {
		return Snapshot{}, err
	}
	defer f.Close()
	var s Snapshot
	if err := json.NewDecoder(io.LimitReader(f, 32768)).Decode(&s); err != nil {
		return s, err
	}
	if s.UpdatedAt.IsZero() {
		return s, errors.New("invalid gateway status")
	}
	if (s.Phase == "active" || s.Phase == "waiting") && time.Since(s.UpdatedAt) > 15*time.Second {
		s.Phase = "stale"
		s.Message = "管理员进程状态已过期，请检查接管进程"
	}
	return s, nil
}
