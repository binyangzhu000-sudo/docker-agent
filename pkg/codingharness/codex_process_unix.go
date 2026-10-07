//go:build unix

package codingharness

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

func configureCodexProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var once sync.Once
	var cancelErr error
	cmd.Cancel = func() error {
		once.Do(func() {
			// A tool subprocess may still hold stdout open after Codex is canceled.
			cancelErr = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(cancelErr, syscall.ESRCH) {
				cancelErr = os.ErrProcessDone
			}
		})
		return cancelErr
	}
}
