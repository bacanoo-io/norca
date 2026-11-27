// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/spf13/cobra"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
)

// errReported is the error of a command that has written its diagnostics
// already: the program exits with an error and writes nothing more.
var errReported = errors.New("reported")

// newModuleCommand is rigger module and its commands.
func (a *app) newModuleCommand() *cobra.Command {
	return newGroup(&cobra.Command{
		Use:   "module",
		Short: "Work with a module: render, test, validate, publish",
		Long: `The commands of a module, in the directory dir (default: the current one).
None of them needs a cluster.`,
	}, a.newRenderCommand(), a.newModuleDirCommand(cmdTest), a.newModuleDirCommand(cmdValidate), a.newPublishCommand())
}

// newModuleDirCommand is rigger module test or module validate: a command
// of one directory, without flags.
func (a *app) newModuleDirCommand(name string) *cobra.Command {
	cmd := &cobra.Command{
		Use:  name + " [dir]",
		Args: oneDirectory(fmt.Errorf("module %s takes one directory", name)),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}

			if name == cmdTest {
				return testModule(cmd.Context(), a.logger, a.stdout, a.stderr, dir)
			}

			return validateModule(cmd.Context(), a.logger, a.stdout, a.stderr, dir)
		},
	}

	if name == cmdTest {
		cmd.Short = "Run the tests of a module"
		cmd.Long = `Run the tests of the module in dir (tests/*.riggertest.hcl): each puts its
values in and asserts over the resources the module renders. A line per
test, the failures under it, and a summary; a failed test fails the command.`
		cmd.Example = `  rigger module test
  rigger module test modules/web`
	} else {
		cmd.Short = "Check a module without a cluster"
		cmd.Long = `Check the module in dir without a cluster: its files, its icon, its tests,
and its example values rendered, the jobs decoded as Norca decodes them.`
		cmd.Example = `  rigger module validate
  rigger module validate modules/web`
	}

	return cmd
}

// oneDirectory accepts at most one argument, the directory of the command;
// more is err.
func oneDirectory(err error) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) > 1 {
			return err
		}

		return nil
	}
}

// validateModule loads the module in dir and validates it; the examples are
// rendered with the job decoder of Norca.
func validateModule(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir string) error {
	eng, err := rigger.New(
		rigger.WithLogger(logger),
		rigger.WithJobDecoder(norcaapi.JobDecoder{}),
		rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}),
		rigger.WithKinds(norcaapi.Kinds()...),
	)
	if err != nil {
		return err
	}

	m, diags, err := loadModule(ctx, eng, dir)
	if err != nil {
		return err
	}

	if !diags.HasErrors() {
		diags = append(diags, eng.ValidateModule(ctx, m)...)
	}

	if len(diags) > 0 {
		if err := writeDiags(stderr, dir, diags); err != nil {
			return err
		}
	}

	if diags.HasErrors() {
		return errReported
	}

	md := m.Metadata()
	_, err = fmt.Fprintf(stdout, "module %s %s in %s is valid\n", md.Name, md.Version, dir)

	return err
}

// loadModule loads the module in dir, with the root it may read child
// modules from: the git repository dir is in, or dir alone outside one.
func loadModule(ctx context.Context, eng *rigger.Engine, dir string) (*rigger.Module, hcl.Diagnostics, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, nil, err
	}

	if !info.IsDir() {
		return nil, nil, fmt.Errorf("%s is not a directory", dir)
	}

	root, rel, err := moduleRoot(dir)
	if err != nil {
		return nil, nil, err
	}

	m, diags := eng.LoadModuleAt(ctx, os.DirFS(root), rel)

	return m, diags, nil
}

// moduleRoot returns the root of the module in dir, and the directory of
// the module in it, slash separated.
func moduleRoot(dir string) (root, rel string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}

	for d := abs; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			rel, err := filepath.Rel(d, abs)
			if err != nil {
				return "", "", err
			}

			return d, filepath.ToSlash(rel), nil
		}

		parent := filepath.Dir(d)
		if parent == d {
			return abs, ".", nil
		}

		d = parent
	}
}

// writeDiags writes diagnostics for a person, with the lines of the files
// they name, which are relative to dir.
func writeDiags(w io.Writer, dir string, diags hcl.Diagnostics) error {
	if _, err := fmt.Fprintf(w, "In %s:\n", dir); err != nil {
		return err
	}

	return hcl.NewDiagnosticTextWriter(w, sources(dir, diags), 0, false).WriteDiagnostics(withoutSensitive(diags))
}

// writeDiagsOf writes diagnostics as writeDiags, of files that are not
// where the user has them: named from the directory rel of root, under a
// heading that says what they are.
func writeDiagsOf(w io.Writer, heading, root, rel string, diags hcl.Diagnostics) error {
	if _, err := fmt.Fprintf(w, "In %s:\n", heading); err != nil {
		return err
	}

	files := map[string]*hcl.File{}
	readSources(files, root, rel, diags)

	return hcl.NewDiagnosticTextWriter(w, files, 0, false).WriteDiagnostics(withoutSensitive(diags))
}

// withoutSensitive returns diags without the context of those whose
// expression reads a sensitive value: the writer of HCL prints the values an
// expression reads, and panics on a marked one.
func withoutSensitive(diags hcl.Diagnostics) hcl.Diagnostics {
	out := make(hcl.Diagnostics, 0, len(diags))

	for _, d := range diags {
		if readsSensitive(d) {
			c := *d
			c.EvalContext = nil
			d = &c
		}

		out = append(out, d)
	}

	return out
}

// readsSensitive reports whether the expression of d reads a sensitive
// value in its context.
func readsSensitive(d *hcl.Diagnostic) bool {
	if d.Expression == nil || d.EvalContext == nil {
		return false
	}

	for _, t := range d.Expression.Variables() {
		if v, diags := t.TraverseAbs(d.EvalContext); !diags.HasErrors() && v.ContainsMarked() {
			return true
		}
	}

	return false
}

// sources reads the files diagnostics name, for the lines they show: the
// files of the module and of its children, named from dir, inside the root
// of the module; the files of values inside the working directory. A file
// that can't be read is shown without its lines.
func sources(dir string, diags hcl.Diagnostics) map[string]*hcl.File {
	files := map[string]*hcl.File{}

	if root, rel, err := moduleRoot(dir); err == nil {
		readSources(files, root, rel, diags)
	}

	readSources(files, ".", ".", diags)

	return files
}

// readSources reads into files the files of diags not read yet, named from
// the directory rel of root, if they are inside root.
func readSources(files map[string]*hcl.File, root, rel string, diags hcl.Diagnostics) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return
	}
	defer r.Close() //nolint:errcheck // read only: closing can lose nothing

	for _, d := range diags {
		if d.Subject == nil || files[d.Subject.Filename] != nil {
			continue
		}

		if src, err := r.ReadFile(path.Join(rel, d.Subject.Filename)); err == nil {
			files[d.Subject.Filename] = &hcl.File{Bytes: src}
		}
	}
}
