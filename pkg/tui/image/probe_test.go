//go:build !windows

package image

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupportsKittyGraphics(t *testing.T) {
	t.Parallel()

	t.Run("non-terminal", func(t *testing.T) {
		file, err := os.Open(os.DevNull)
		require.NoError(t, err)
		defer file.Close()

		start := time.Now()
		assert.False(t, SupportsKittyGraphics(file, file))
		assert.Less(t, time.Since(start), kittyProbeTimeout)
	})

	t.Run("supported", func(t *testing.T) {
		ptmx, tty, err := pty.Open()
		require.NoError(t, err)
		defer ptmx.Close()
		defer tty.Close()

		go answerKittyProbe(ptmx, kittyProbeOK)
		assert.True(t, SupportsKittyGraphics(tty, tty))
	})

	t.Run("unsupported", func(t *testing.T) {
		ptmx, tty, err := pty.Open()
		require.NoError(t, err)
		defer ptmx.Close()
		defer tty.Close()

		go answerKittyProbe(ptmx, "\x1b_Gi=4242;ENOTSUP\x1b\\")
		start := time.Now()
		assert.False(t, SupportsKittyGraphics(tty, tty))
		assert.Less(t, time.Since(start), kittyProbeTimeout)
	})
}

func TestKittyProbeResponse(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		response  string
		supported bool
		answered  bool
	}{
		{name: "supported", response: kittyProbeOK, supported: true, answered: true},
		{name: "unsupported", response: "\x1b_Gi=4242;ENOTSUP\x1b\\", answered: true},
		{name: "error", response: "\x1b_Gi=4242;EINVAL: invalid query\x1b\\", answered: true},
		{name: "wrong ID", response: "\x1b_Gi=42;OK\x1b\\"},
		{name: "empty", response: "\x1b_Gi=4242;\x1b\\"},
		{name: "incomplete", response: "\x1b_Gi=4242;OK\x1b"},
		{name: "silence"},
		{name: "unrelated reply", response: "\x1b_Gi=42;OK\x1b\\" + kittyProbeOK, supported: true, answered: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			supported, answered := kittyProbeResponse([]byte(tt.response))
			assert.Equal(t, tt.supported, supported)
			assert.Equal(t, tt.answered, answered)
		})
	}
}

func TestKittyProbeResponseFragmented(t *testing.T) {
	t.Parallel()
	for _, response := range []string{kittyProbeOK, "\x1b_Gi=4242;ENOTSUP\x1b\\"} {
		for end := range len(response) {
			_, answered := kittyProbeResponse([]byte(response[:end]))
			assert.False(t, answered, "partial reply %q", response[:end])
		}
		_, answered := kittyProbeResponse([]byte(response))
		assert.True(t, answered)
	}
}

func answerKittyProbe(terminal *os.File, response string) {
	var seen []byte
	buf := make([]byte, 128)
	for {
		n, err := terminal.Read(buf)
		if err != nil {
			return
		}
		seen = append(seen, buf[:n]...)
		if bytes.Contains(seen, []byte(kittyProbeQuery)) {
			_, _ = terminal.WriteString(response)
			return
		}
	}
}

func TestSupportsKittyGraphicsFragmentedReply(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{"OK", "ENOTSUP"} {
		t.Run(payload, func(t *testing.T) {
			ptmx, tty, err := pty.Open()
			require.NoError(t, err)
			defer ptmx.Close()
			defer tty.Close()
			go func() {
				answerKittyProbe(ptmx, "\x1b_Gi=4242;"+payload+"\x1b")
				time.Sleep(10 * time.Millisecond) //nolint:forbidigo // Force the ST terminator across actual terminal reads.
				_, _ = ptmx.WriteString("\\")
			}()
			assert.Equal(t, payload == "OK", SupportsKittyGraphics(tty, tty))
		})
	}
}
