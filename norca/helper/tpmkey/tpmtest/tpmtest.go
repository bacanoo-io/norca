// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package tpmtest runs a software TPM (swtpm) for the tests of TPM keys.
// A test that needs it is skipped where swtpm is not installed.
package tpmtest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-tpm/tpm2/transport"
)

// Start runs swtpm for the test, stopped at its end, and returns an opener
// of it. Every command is sent on a connection of its own, so any number of
// openers may be open at once.
func Start(t testing.TB) func() (transport.TPMCloser, error) {
	t.Helper()
	bin, err := exec.LookPath("swtpm")
	if err != nil {
		t.Skip("swtpm is not installed: the TPM tests are skipped")
	}
	// A short directory: a Unix socket path has at most 108 bytes.
	dir, err := os.MkdirTemp("", "swtpm")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "tpm.sock")
	cmd := exec.CommandContext(context.Background(), bin, "socket", "--tpm2", //nolint:gosec // G204: the swtpm of the test
		"--tpmstate", "dir="+dir,
		"--server", "type=unixio,path="+sock,
		"--ctrl", "type=unixio,path="+filepath.Join(dir, "ctrl.sock"),
		"--flags", "not-need-init,startup-clear")
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting swtpm: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.RemoveAll(dir)
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c, err := (&net.Dialer{}).DialContext(context.Background(), "unix", sock); err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("swtpm did not listen on %s: %s", sock, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func() (transport.TPMCloser, error) { return &socketTPM{path: sock}, nil }
}

// socketTPM sends each command of the TPM on a new connection to swtpm.
type socketTPM struct {
	path string
}

// Send implements transport.TPM.
func (s *socketTPM) Send(cmd []byte) ([]byte, error) {
	c, err := (&net.Dialer{}).DialContext(context.Background(), "unix", s.path)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.Write(cmd); err != nil {
		return nil, err
	}
	// The response: a tag (2 bytes), its size (4 bytes) and a code.
	head := make([]byte, 10)
	if _, err := io.ReadFull(c, head); err != nil {
		return nil, fmt.Errorf("the response of swtpm: %w", err)
	}
	size := binary.BigEndian.Uint32(head[2:6])
	if size < 10 || size > 1<<16 {
		return nil, errors.New("the response of swtpm has a bad size")
	}
	rsp := make([]byte, size)
	copy(rsp, head)
	if _, err := io.ReadFull(c, rsp[10:]); err != nil {
		return nil, fmt.Errorf("the response of swtpm: %w", err)
	}
	return rsp, nil
}

// Close implements transport.TPMCloser.
func (s *socketTPM) Close() error { return nil }

type lockedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
