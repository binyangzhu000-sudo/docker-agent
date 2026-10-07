//go:build unix

package codingharness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
)

func TestCodexDescendantCleanup(t *testing.T) {
	for _, tt := range []struct {
		name, script, want string
		cancel             bool
	}{
		{name: "cancel", script: "sleep 60 &\necho $! > %q\nprintf '%%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread\"}'\nwait\n", cancel: true},
		{name: "malformed after leader exit", script: "sleep 60 &\necho $! > %q\necho '{broken'\n", want: "invalid Codex JSON event"},
		{name: "active stdout after leader exit", script: "(while :; do echo '{\"type\":\"future.event\"}'; sleep 0.1; done) 2>/dev/null &\necho $! > %q\nprintf '%%s\\n' '" + strings.TrimSpace(codexCompleted) + "'\n", want: "stdout remained open"},
		{name: "inherited stderr", script: "sleep 60 >/dev/null &\necho $! > %q\nprintf '%%s\\n' '" + strings.TrimSpace(codexCompleted) + "'\n", want: "WaitDelay"},
		{name: "early stdout EOF", script: "echo $$ > %q\nexec 1>&-\nexec sleep 60\n", want: "without turn.completed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useCodexCLI(t, "", 0)
			dir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
			pidPath := filepath.Join(dir, "child.pid")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n"+fmt.Sprintf(tt.script, pidPath)), 0o700))
			t.Cleanup(func() {
				data, err := os.ReadFile(pidPath)
				if err == nil {
					pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
					if err == nil {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			})
			p, err := Factory(&latest.HarnessConfig{Type: TypeCodex})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			watchdog, stop := context.WithTimeout(ctx, 30*time.Second)
			defer stop()
			err = p.Run(watchdog, "test", func(ev harness.Event) {
				if tt.cancel && ev.Type == harness.EventSessionID {
					cancel()
				}
			})
			if tt.cancel {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorContains(t, err, tt.want)
				require.NoError(t, watchdog.Err())
			}
			data, err := os.ReadFile(pidPath)
			require.NoError(t, err)
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				// Killed children can remain as zombies until the OS reaps them.
				if err := syscall.Kill(pid, 0); err != nil {
					return true
				}
				out, err := exec.CommandContext(t.Context(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
				return err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
			}, 5*time.Second, 20*time.Millisecond)
		})
	}
}
