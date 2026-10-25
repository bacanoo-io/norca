package seal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// externalDefaultTimeout bounds each wrap/unwrap invocation when none is given.
const externalDefaultTimeout = 30 * time.Second

// External wraps the master key by delegating to an operator-supplied command,
// so any cloud KMS or HSM is reachable without a provider SDK in the vault (ADR
// D-015, docs/design/kms-hsm-seal.md). The command is invoked in two modes:
//
//	<command> [args...] wrap     # plaintext master key on stdin -> wrapped blob on stdout
//	<command> [args...] unwrap   # wrapped blob on stdin -> plaintext master key on stdout
//
// Success is exit status 0 with non-empty stdout; the wrapped blob is opaque and
// stored/passed back verbatim. A non-zero exit or a timeout fails the operation
// (the vault stays sealed — never fail-open).
type External struct {
	command string
	args    []string
	env     []string // appended to the inherited environment, if non-empty
	timeout time.Duration
}

// NewExternal returns an external-command seal. args are passed before the
// wrap/unwrap mode; env, if non-empty, is appended to the inherited environment
// for the command (provider credentials otherwise come from the ambient
// environment). A timeout <= 0 uses [externalDefaultTimeout].
func NewExternal(command string, args, env []string, timeout time.Duration) *External {
	if timeout <= 0 {
		timeout = externalDefaultTimeout
	}
	return &External{command: command, args: args, env: env, timeout: timeout}
}

// Type implements [Seal].
func (e *External) Type() string { return "external" }

// Wrap implements [Seal].
func (e *External) Wrap(ctx context.Context, plaintext []byte) ([]byte, error) {
	return e.run(ctx, "wrap", plaintext)
}

// Unwrap implements [Seal].
func (e *External) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	return e.run(ctx, "unwrap", wrapped)
}

func (e *External) run(ctx context.Context, mode string, input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	args := append(append([]string(nil), e.args...), mode)
	// G204: the command and args are operator-supplied server configuration (the
	// -seal-external-* flags), not untrusted input — running a configured command
	// is the entire point of a pluggable KMS/HSM seal.
	cmd := exec.CommandContext(ctx, e.command, args...) //nolint:gosec
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if len(e.env) > 0 {
		cmd.Env = append(os.Environ(), e.env...)
	}

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("seal: external %s command timed out after %s", mode, e.timeout)
		}
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) > 0 {
			return nil, fmt.Errorf("seal: external %s command failed: %w: %s", mode, err, msg)
		}
		return nil, fmt.Errorf("seal: external %s command failed: %w", mode, err)
	}
	// The bytes are passed through verbatim — a wrapped blob or the master key —
	// so the format is entirely the command's own; the vault imposes none.
	out := stdout.Bytes()
	if len(out) == 0 {
		return nil, fmt.Errorf("seal: external %s command produced no output", mode)
	}
	return out, nil
}
