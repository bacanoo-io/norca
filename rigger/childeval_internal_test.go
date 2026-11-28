// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/rigger/internal/evalwire"
)

// silentProcess reads requests and never answers, until it is closed.
type silentProcess struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	closed chan struct{}
}

func newSilentProcess() *silentProcess {
	r, w := io.Pipe()
	return &silentProcess{r: r, w: w, closed: make(chan struct{})}
}

func (p *silentProcess) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *silentProcess) Write(b []byte) (int, error) { return len(b), nil }

func (p *silentProcess) Close() error {
	select {
	case <-p.closed:
	default:
		close(p.closed)
		_ = p.w.CloseWithError(errors.New("killed"))
	}

	return nil
}

func (p *silentProcess) Wait() error {
	<-p.closed

	return errors.New("signal: killed")
}

// A child that does not answer within the limit is ended, and the call
// says so; the engine does not use it again.
func TestChildEvaluator_TimeLimit(t *testing.T) { //nolint:paralleltest // it shortens transferTime
	old := transferTime
	transferTime = 20 * time.Millisecond

	t.Cleanup(func() { transferTime = old })

	var proc *silentProcess

	c := &childEvaluator{start: func(context.Context) (EvaluationProcess, error) {
		proc = newSilentProcess()

		return proc, nil
	}}

	_, err := c.call(t.Context(), 10*time.Millisecond, &evalwire.Request{})
	if !errors.Is(err, ErrEvaluationProcess) || !strings.Contains(err.Error(), "no answer within 30ms") {
		t.Fatalf("err = %v, want no answer within the limit", err)
	}

	select {
	case <-proc.closed:
	default:
		t.Fatal("the silent child was not ended")
	}

	if c.proc != nil {
		t.Fatal("the ended child is still the engine's")
	}
}
