// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package evalwire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/resource"
)

// MaxFrame bounds one frame: the files of a stack within the limits of an
// engine, and the resources of its evaluation, fit many times.
const MaxFrame = 256 << 20

// ErrFrameTooLarge is a frame past MaxFrame.
var ErrFrameTooLarge = errors.New("a frame past the bound of the evaluation protocol")

// Limits are the limits of the engine the child evaluates with.
type Limits struct {
	ModuleBytes int64         `json:"module_bytes"`
	FileBytes   int64         `json:"file_bytes"`
	Resources   int           `json:"resources"`
	Depth       int           `json:"depth"`
	Modules     int           `json:"modules"`
	Duration    time.Duration `json:"duration"`
}

// Input is a value of a variable with where it was written.
type Input struct {
	Value Value      `json:"value"`
	Range *hcl.Range `json:"range,omitempty"`
}

// Facts are the rigger.* of a module evaluated on its own.
type Facts struct {
	Release       string `json:"release"`
	Namespace     string `json:"namespace"`
	Region        string `json:"region"`
	ModuleName    string `json:"module_name"`
	ModuleVersion string `json:"module_version"`
}

// Request is one evaluation: of a stack or of a module.
type Request struct {
	Limits Limits         `json:"limits"`
	Stack  *StackRequest  `json:"stack,omitempty"`
	Module *ModuleRequest `json:"module,omitempty"`
}

// StackRequest evaluates a stack with the values of its variables; Existing
// are the generated values the stack has already.
type StackRequest struct {
	Tree     *StackTree        `json:"tree"`
	Values   map[string]Input  `json:"values"`
	Existing map[string]string `json:"existing,omitempty"`
}

// ModuleRequest evaluates a module for an install.
type ModuleRequest struct {
	Tree   *ModuleTree      `json:"tree"`
	Values map[string]Input `json:"values"`
	Facts  Facts            `json:"facts"`
}

// Response is the answer of a request: the diagnostics, and the result of
// an evaluation without errors.
type Response struct {
	Diagnostics []Diagnostic    `json:"diagnostics,omitempty"`
	Stack       *StackResponse  `json:"stack,omitempty"`
	Module      *ModuleResponse `json:"module,omitempty"`
}

// StackResponse is an evaluated stack; Generated are the values it
// generated now, by name.
type StackResponse struct {
	Releases  []Release         `json:"releases"`
	Secrets   []Secret          `json:"secrets,omitempty"`
	Used      []string          `json:"used,omitempty"`
	Missing   []string          `json:"missing,omitempty"`
	Outputs   []Output          `json:"outputs,omitempty"`
	Generated map[string]string `json:"generated,omitempty"`
}

// Release is an evaluated release of a stack. Block is the label of its
// module block, Part the part it is of (empty for one of the stack).
type Release struct {
	Block     string   `json:"block"`
	Part      string   `json:"part,omitempty"`
	Name      string   `json:"name"`
	Enabled   bool     `json:"enabled"`
	Namespace string   `json:"namespace"`
	Values    Value    `json:"values"`
	Vars      Value    `json:"vars"`
	Result    *Result  `json:"result,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	Pending   bool     `json:"pending,omitempty"`
}

// Secret is a secret of secrets.hcl.
type Secret struct {
	Path      string            `json:"path"`
	Items     map[string]string `json:"items,omitempty"`
	Overwrite bool              `json:"overwrite,omitempty"`
	Pending   bool              `json:"pending,omitempty"`
}

// ModuleResponse is an evaluated module and the object of its variables.
type ModuleResponse struct {
	Vars   Value   `json:"vars"`
	Result *Result `json:"result"`
}

// Result is the result of an evaluated module.
type Result struct {
	Resources    []Resource `json:"resources"`
	Outputs      []Output   `json:"outputs,omitempty"`
	OutputValues Value      `json:"output_values"`
}

// Resource is an evaluated resource; Secrets are its secret items in
// plain text: the pipe is between two processes of one host.
type Resource struct {
	Kind    resource.Kind     `json:"kind"`
	Name    string            `json:"name"`
	Spec    json.RawMessage   `json:"spec,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	Hook    *resource.Hook    `json:"hook,omitempty"`
}

// Output is an evaluated output.
type Output struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Value       json.RawMessage `json:"value,omitempty"`
	Sensitive   bool            `json:"sensitive,omitempty"`
}

// WriteFrame writes v as one frame: its length as four bytes, big endian,
// then its JSON.
func WriteFrame(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	if len(data) > MaxFrame {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(data))
	}

	var head [4]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(data))) //nolint:gosec // G115: bounded by MaxFrame above

	if _, err := w.Write(head[:]); err != nil {
		return err
	}

	_, err = w.Write(data)

	return err
}

// ReadFrame reads one frame into v; io.EOF when the other end closed
// before a frame.
func ReadFrame(r io.Reader, v any) error {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return err
	}

	n := binary.BigEndian.Uint32(head[:])
	if n > MaxFrame {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, n)
	}

	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}

		return err
	}

	return json.Unmarshal(data, v)
}
