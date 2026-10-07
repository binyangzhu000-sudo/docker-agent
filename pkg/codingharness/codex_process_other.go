//go:build !unix

package codingharness

import "os/exec"

func configureCodexProcess(_ *exec.Cmd) {}
