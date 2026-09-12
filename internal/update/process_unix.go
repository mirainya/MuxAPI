//go:build !windows

package update

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

func waitForParentExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("等待旧进程退出失败: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("等待旧进程退出超时")
}
