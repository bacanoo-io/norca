// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	rig "github.com/bacanoo-io/norca-rig"
	"github.com/bacanoo-io/norca-rig/cli/norcaapi"
)

// testModule runs the tests of the module in dir with the job decoder of
// Norca: a line per test, the failures under it, and a summary. A failed
// test is an error.
func testModule(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir string) error {
	eng, err := rig.New(rig.WithLogger(logger), rig.WithJobDecoder(norcaapi.JobDecoder{}))
	if err != nil {
		return err
	}

	m, diags, err := loadModule(ctx, eng, dir)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, dir, diags)
	}

	results, diags := eng.TestModule(ctx, m)
	if diags.HasErrors() {
		return reportDiags(stderr, dir, diags)
	}

	failed := 0

	for _, r := range results {
		if err := writeResult(stdout, stderr, dir, r); err != nil {
			return err
		}

		if !r.Passed() {
			failed++
		}
	}

	if _, err := fmt.Fprintf(stdout, "%d passed, %d failed\n", len(results)-failed, failed); err != nil {
		return err
	}

	if failed > 0 {
		return errReported
	}

	return nil
}

// writeResult writes the line of a test, and why it failed.
func writeResult(stdout, stderr io.Writer, dir string, r rig.TestResult) error {
	status := "ok  "
	if !r.Passed() {
		status = "FAIL"
	}

	if _, err := fmt.Fprintf(stdout, "%s %s: %s\n", status, r.File, r.Name); err != nil {
		return err
	}

	for _, f := range r.Failures {
		if _, err := fmt.Fprintf(stdout, "     %s\n", f); err != nil {
			return err
		}
	}

	if len(r.Diagnostics) > 0 {
		return writeDiags(stderr, dir, r.Diagnostics)
	}

	return nil
}
