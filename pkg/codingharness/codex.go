package codingharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/portcullis"
	baseharness "github.com/rumpl/harness"

	"github.com/docker/docker-agent/pkg/chat"
)

const (
	maxCodexEventBytes      = 16 * 1024 * 1024
	maxCodexDiagnosticBytes = 4096
	codexDrainTimeout       = 2 * time.Second
)

type codexProvider struct {
	baseharness.Provider
}

func (p codexProvider) Run(ctx context.Context, prompt string, handle func(baseharness.Event)) error {
	return p.run(ctx, p.PrintCommand(prompt), handle)
}

func (p codexProvider) Resume(ctx context.Context, sessionID, prompt string, handle func(baseharness.Event)) error {
	if sessionID == "" {
		return errors.New("resume: session ID is empty")
	}
	resumable, ok := p.Provider.(baseharness.ResumableProvider)
	if !ok {
		return errors.New("codex provider does not support sessions")
	}
	return p.run(ctx, resumable.ResumeCommand(sessionID, prompt), handle)
}

func (p codexProvider) run(ctx context.Context, command string, handle func(baseharness.Event)) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Replace the shell so cancellation reaches the CLI, not just its launcher.
	cmd := exec.CommandContext(runCtx, "sh", "-c", "exec "+command)
	configureCodexProcess(cmd)
	cmd.WaitDelay = codexDrainTimeout
	stdout, output, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("codex stdout pipe: %w", err)
	}
	defer stdout.Close()
	cmd.Stdout = output
	var stderr codexDiagnosticWriter
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = output.Close()
		return fmt.Errorf("start codex: %w", err)
	}
	_ = output.Close()
	waited := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		close(exited)
		waited <- err
	}()
	stream := &codexJSONWriter{provider: p.Provider, handle: handle, cancel: cancel}
	readErr := readCodexOutput(runCtx, stdout, exited, stream)
	if readErr == nil {
		readErr = stream.finish()
	}
	if readErr == nil && !stream.completed {
		readErr = errors.New("codex stream ended without turn.completed")
		if stream.lastError != "" {
			readErr = fmt.Errorf("%w: %s", readErr, stream.lastError)
		}
	}
	if readErr != nil {
		// The leader may already have exited, so exec's context watcher alone
		// cannot guarantee that descendants holding stdout are terminated.
		_ = cmd.Cancel()
		cancel()
	}
	waitErr := <-waited
	if err := ctx.Err(); err != nil {
		return err
	}
	if stream.err != nil {
		return stream.err
	}
	if readErr != nil {
		if detail := codexDiagnostic(stderr.String()); detail != "" {
			return fmt.Errorf("read codex JSON stream: %w: %s", readErr, detail)
		}
		return fmt.Errorf("read codex JSON stream: %w", readErr)
	}
	if waitErr != nil {
		_ = cmd.Cancel()
		if detail := codexDiagnostic(stderr.String()); detail != "" {
			return fmt.Errorf("codex process: %w: %s", waitErr, detail)
		}
		return fmt.Errorf("codex process: %w", waitErr)
	}
	return nil
}

func readCodexOutput(ctx context.Context, stdout *os.File, exited <-chan struct{}, stream *codexJSONWriter) error {
	type readResult struct {
		data []byte
		err  error
	}
	reads := make(chan readResult)
	stop := make(chan struct{})
	readDone := make(chan struct{})
	defer func() {
		close(stop)
		_ = stdout.Close()
		<-readDone
	}()
	go func() {
		defer close(readDone)
		for {
			data := make([]byte, 32*1024)
			n, err := stdout.Read(data)
			select {
			case reads <- readResult{data: data[:n], err: err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var deadline <-chan time.Time
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	remaining := codexDrainTimeout
	var draining bool
	for {
		start := time.Now()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			exited = nil
			draining = true
			timer.Reset(remaining)
			deadline = timer.C
		case <-deadline:
			return errors.New("codex stdout remained open after process exit")
		case result := <-reads:
			if draining {
				remaining -= time.Since(start)
				timer.Stop()
			}
			// Event delivery is synchronous; consumer back-pressure is not pipe idle time.
			if _, err := stream.Write(result.data); err != nil {
				return err
			}
			if result.err != nil {
				if errors.Is(result.err, io.EOF) {
					return nil
				}
				return result.err
			}
			if draining {
				if remaining <= 0 {
					return errors.New("codex stdout remained open after process exit")
				}
				timer.Reset(remaining)
			}
		}
	}
}

type codexJSONWriter struct {
	provider  baseharness.Provider
	handle    func(baseharness.Event)
	cancel    context.CancelFunc
	pending   []byte
	completed bool
	lastError string
	err       error
}

func (w *codexJSONWriter) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 {
		line, rest, found := bytes.Cut(data, []byte{'\n'})
		if len(line) > maxCodexEventBytes-len(w.pending) {
			return 0, w.fail(fmt.Errorf("codex JSON event exceeds %d bytes", maxCodexEventBytes))
		}
		w.pending = append(w.pending, line...)
		if !found {
			break
		}
		if err := w.event(w.pending); err != nil {
			return 0, w.fail(err)
		}
		w.pending = w.pending[:0]
		data = rest
	}
	return n, nil
}

func (w *codexJSONWriter) fail(err error) error {
	w.err = err
	w.cancel()
	return err
}

func (w *codexJSONWriter) finish() error {
	if len(w.pending) == 0 {
		return nil
	}
	return w.event(w.pending)
}

func (w *codexJSONWriter) event(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var event struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &event); err != nil {
		return fmt.Errorf("invalid Codex JSON event: %w", err)
	}
	if event.Type == "" {
		return errors.New("invalid Codex JSON event: missing event type")
	}
	switch event.Type {
	case "turn.started":
		w.completed = false
		w.lastError = ""
	case "turn.completed":
		w.completed = true
		w.lastError = ""
	case "error":
		// Reconnection attempts also emit error events; turn.failed is terminal.
		var message string
		_ = json.Unmarshal(event.Message, &message)
		w.lastError = codexDiagnostic(message)
	case "turn.failed":
		var failure struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(event.Error, &failure)
		detail := codexDiagnostic(failure.Message)
		if detail == "" {
			detail = "no error detail provided"
		}
		return fmt.Errorf("codex turn failed: %s", detail)
	}
	for _, ev := range w.provider.ParseStreamLine(string(line)) {
		w.handle(ev)
	}
	return nil
}

type codexDiagnosticWriter struct {
	strings.Builder

	truncated bool
}

func (w *codexDiagnosticWriter) Write(data []byte) (int, error) {
	n := len(data)
	remaining := maxCodexDiagnosticBytes - w.Len()
	if n > remaining {
		w.truncated = true
	}
	if remaining > 0 {
		_, _ = w.Builder.Write(data[:min(n, remaining)])
	}
	return n, nil
}

func (w *codexDiagnosticWriter) String() string {
	// A truncated credential may no longer be recognizable by the redactor.
	if w.truncated {
		return "stderr exceeded diagnostic limit"
	}
	return w.Builder.String()
}

func codexDiagnostic(message string) string {
	return chat.TruncateUTF8Bytes(portcullis.Redact(strings.Join(strings.Fields(message), " ")), maxCodexDiagnosticBytes)
}
