//go:build windows

package update

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

const (
	synchronize      = 0x00100000
	waitObject0      = 0x00000000
	waitTimeout      = 0x00000102
	processQueryInfo = 0x0400
)

func waitForParentExit(pid int, timeout time.Duration) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	openProcess := kernel32.NewProc("OpenProcess")
	waitForSingleObject := kernel32.NewProc("WaitForSingleObject")
	closeHandle := kernel32.NewProc("CloseHandle")
	handle, _, callErr := openProcess.Call(uintptr(synchronize|processQueryInfo), 0, uintptr(pid))
	if handle == 0 {
		if callErr != nil && !errors.Is(callErr, syscall.Errno(87)) {
			return fmt.Errorf("打开旧进程失败: %w", callErr)
		}
		return nil
	}
	defer closeHandle.Call(handle)
	result, _, _ := waitForSingleObject.Call(handle, uintptr(timeout/time.Millisecond))
	switch result {
	case waitObject0:
		return nil
	case waitTimeout:
		return errors.New("等待旧进程退出超时")
	default:
		return errors.New("等待旧进程退出失败")
	}
}
