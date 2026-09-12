package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ApplyManifest is invoked by the copied helper process, not by the server
// process. Keeping replacement outside the running executable makes Windows
// replacement possible and gives both platforms a single rollback path.
func ApplyManifest(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if m.TargetPath == "" || m.StagedPath == "" || m.ParentPID <= 0 {
		return errors.New("更新清单不完整")
	}
	target, err := filepath.Abs(m.TargetPath)
	if err != nil {
		return err
	}
	staged, err := filepath.Abs(m.StagedPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(staged); err != nil {
		return fmt.Errorf("更新文件不存在: %w", err)
	}
	if err := waitForParentExit(m.ParentPID, 45*time.Second); err != nil {
		return err
	}

	backup := target + ".bak.update-" + time.Now().UTC().Format("20060102_150405")
	if err := os.Rename(target, backup); err != nil {
		return fmt.Errorf("备份旧程序失败: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(backup, target)
		return fmt.Errorf("替换程序失败: %w", err)
	}
	if err := makeExecutable(target); err != nil {
		_ = os.Remove(target)
		_ = os.Rename(backup, target)
		return err
	}

	child, err := startProgram(target, m.Args, m.WorkingDir)
	if err != nil {
		_ = os.Remove(target)
		_ = os.Rename(backup, target)
		return fmt.Errorf("启动新程序失败: %w", err)
	}
	if m.HealthURL != "" {
		if err := waitHealth(m.HealthURL, 45*time.Second); err != nil {
			_ = child.Kill()
			_, _ = child.Wait()
			_ = os.Remove(target)
			if restoreErr := os.Rename(backup, target); restoreErr != nil {
				return fmt.Errorf("新程序健康检查失败，恢复旧程序也失败: %v; %w", restoreErr, err)
			}
			_, _ = startProgram(target, m.Args, m.WorkingDir)
			return fmt.Errorf("新程序健康检查失败，已恢复旧版本: %w", err)
		}
	}
	_ = os.Remove(backup)
	_ = os.Remove(m.StagedPath)
	_ = os.Remove(m.TempDir + string(os.PathSeparator) + filepath.Base(m.StagedPath))
	return nil
}

func startProgram(path string, args []string, workingDir string) (*os.Process, error) {
	cmd := exec.Command(path, args...)
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	if err := cmd.Start(); err != nil {
		devNull.Close()
		return nil, err
	}
	_ = devNull.Close()
	return cmd.Process, nil
}

func waitHealth(endpoint string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err == nil {
			resp, requestErr := client.Do(req)
			if requestErr == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					cancel()
					return nil
				}
			}
		}
		cancel()
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("健康检查超时")
}

func makeExecutable(path string) error {
	if err := os.Chmod(path, 0o755); err != nil && !strings.Contains(strings.ToLower(err.Error()), "not supported") {
		return err
	}
	return nil
}
