// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rig

import (
	"context"
	"io/fs"
	"log/slog"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca-rig/internal/eval"
	"github.com/bacanoo-io/norca-rig/oplog"
)

// TestResult is the result of a test of a module.
type TestResult struct {
	// File is the test file under tests/; Name the label of the test.
	File string `json:"file"`
	Name string `json:"name"`
	// Failures say why the test failed; none when it passed.
	Failures []string `json:"failures,omitempty"`
	// Diagnostics are those of an evaluation that failed unexpectedly, or
	// of an assertion that could not be evaluated.
	Diagnostics hcl.Diagnostics `json:"-"`
}

// Passed reports whether the test passed.
func (r TestResult) Passed() bool {
	return len(r.Failures) == 0
}

// TestModule runs the tests of m, the test blocks of its
// tests/*.rigtest.hcl (design 7.4): each evaluates the module with its
// values and checks its assertions over resource.<block>.<name> and
// output.<name>, or expects an error. No cluster is needed; jobs go through
// the job decoder of the engine. The diagnostics are those of test files
// that do not parse.
func (e *Engine) TestModule(ctx context.Context, m *Module) ([]TestResult, hcl.Diagnostics) {
	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "module.test",
		"module", m.mod.Metadata.Name, "version", m.mod.Metadata.Version)

	var (
		results []TestResult
		diags   hcl.Diagnostics
	)

	for _, p := range m.mod.Tests {
		tests, moreDiags := e.parseTests(m, p)
		diags = append(diags, moreDiags...)

		for _, t := range tests {
			if err := ctx.Err(); err != nil {
				diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Tests cancelled", Detail: err.Error() + "."})
				span.End(ctx, diagsErr(diags))

				return results, diags
			}

			results = append(results, e.runTest(ctx, m, p, t))
		}
	}

	span.End(ctx, diagsErr(diags))

	return results, diags
}

// parseTests reads and parses the test file at p.
func (e *Engine) parseTests(m *Module, p string) ([]*eval.Test, hcl.Diagnostics) {
	src, err := fs.ReadFile(m.fsys, p)
	if err != nil {
		return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Test file not read", Detail: err.Error() + "."}}
	}

	return eval.ParseTests(p, src)
}

// runTest runs one test within the time limit of the engine.
func (e *Engine) runTest(ctx context.Context, m *Module, p string, t *eval.Test) TestResult {
	ctx, span := oplog.Start(ctx, e.logger, slog.LevelDebug, "module.test.run", "file", p, "test", t.Name)

	if e.limits.Duration > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, e.limits.Duration)
		defer cancel()
	}

	out := eval.RunTest(ctx, m.mod, t, eval.Config{
		FileBytes: e.limits.FileBytes, Resources: e.limits.Resources, Jobs: e.jobs,
	})
	span.End(ctx, diagsErr(out.Diagnostics))

	return TestResult{File: p, Name: t.Name, Failures: out.Failures, Diagnostics: out.Diagnostics}
}
