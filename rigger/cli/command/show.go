// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"text/tabwriter"

	"github.com/hashicorp/hcl/v2"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/module"
	"github.com/bacanoo-io/norca/rigger/source"
)

// rigger show describes one module or stack of a registry without a
// cluster: the version an install would take, fetched and verified as an
// install verifies it.

var (
	errShowSource = errors.New("show needs one module or stack: <namespace>/<name> of the global registry, builtin/<name>, a git or oci:// address")
	errShowKind   = errors.New("-kind is module or stack")
)

// showFlags are the flags of rigger show.
type showFlags struct {
	kind    string
	version string
	readme  bool
	json    bool
}

// runShow runs rigger show <source> [flags].
func runShow(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	var positional []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = append(positional, args[0]), args[1:]
	}

	var f showFlags

	flags := flag.NewFlagSet("rigger show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&f.kind, "kind", "", "module or stack (default: a stack when the address names one, as stacks/<name> of a repository or a stack of the global registry)")
	flags.StringVar(&f.version, "version", "", "the version constraint (\"~> 1.2\"); none shows the newest")
	flags.BoolVar(&f.readme, "readme", false, "print the README.md too")
	flags.BoolVar(&f.json, "json", false, "write the details as JSON")

	if err := flags.Parse(args); err != nil {
		return exitError
	}

	positional = append(positional, flags.Args()...)

	return run(ctx, logger, stderr, cmdShow, func(ctx context.Context) error {
		if len(positional) != 1 {
			return errShowSource
		}

		if f.kind != "" && f.kind != string(index.KindModule) && f.kind != string(index.KindStack) {
			return fmt.Errorf("%w, not %q", errShowKind, f.kind)
		}

		eng, err := offlineEngine(logger)
		if err != nil {
			return err
		}

		shown, err := showSource(ctx, eng, stderr, positional[0], f)
		if err != nil {
			return err
		}

		return writeDetails(stdout, shown, f)
	})
}

// shownSource is what rigger show writes of a module or a stack.
type shownSource struct {
	Kind     index.Kind      `json:"kind"`
	Source   string          `json:"source"`
	Version  string          `json:"version"`
	Metadata module.Metadata `json:"metadata"`
	// Versions are the versions the source has, the newest first.
	Versions   []string `json:"versions"`
	Repository string   `json:"repository,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	Commit     string   `json:"commit,omitempty"`
	Digest     string   `json:"digest"`
	// Signers are the key IDs of the signatures that verified the digest.
	Signers []string `json:"signers"`
	// Builtin reports a module built into the program, which is not signed.
	Builtin bool `json:"builtin,omitempty"`
	// Warnings are the advisories of the version and that it is not signed.
	Warnings  []string             `json:"warnings"`
	Variables []module.Variable    `json:"variables"`
	Outputs   []module.Output      `json:"outputs"`
	Modules   []rigger.StackModule `json:"modules,omitempty"`
	Readme    string               `json:"readme,omitempty"`
}

// showSource fetches and verifies the version of the source an install
// would take, writes its warnings to stderr, and describes it.
func showSource(ctx context.Context, eng *rigger.Engine, stderr io.Writer, address string, f showFlags) (*shownSource, error) {
	kind, versions, err := sourceVersions(ctx, eng, address, index.Kind(f.kind))
	if err != nil {
		return nil, err
	}

	var shown *shownSource

	if kind == index.KindStack {
		shown, err = showStack(ctx, eng, stderr, address, f)
	} else {
		shown, err = showModule(ctx, eng, stderr, address, f)
	}

	if err != nil {
		return nil, err
	}

	shown.Kind, shown.Versions = kind, versions

	return shown, nil
}

// sourceVersions returns the kind of the source and its versions, the
// newest first. Without a kind, the directory stacks/<name> of a repository
// is a stack, and a name of the global registry is a module unless the
// registry lists only a stack of it.
func sourceVersions(ctx context.Context, eng *rigger.Engine, address string, kind index.Kind) (index.Kind, []string, error) {
	guessed := kind == ""
	if guessed {
		kind = index.KindModule

		if req, err := source.Parse(address, nil); err == nil && req.Kind == source.KindGit && strings.HasPrefix(req.Path, "stacks/") {
			kind = index.KindStack
		}
	}

	if kind == index.KindStack {
		versions, err := eng.StackVersions(ctx, address)

		return kind, versions, err
	}

	versions, err := eng.ModuleVersions(ctx, address)
	if errors.Is(err, fs.ErrNotExist) && guessed {
		if stackVersions, stackErr := eng.StackVersions(ctx, address); stackErr == nil {
			return index.KindStack, stackVersions, nil
		}
	}

	return kind, versions, err
}

// showModule fetches the newest version of a module that meets the
// constraint, as rigger install does.
func showModule(ctx context.Context, eng *rigger.Engine, stderr io.Writer, address string, f showFlags) (*shownSource, error) {
	m, diags := eng.FetchNewestModule(ctx, address, f.version)
	if err := checkDiags(stderr, address, diags); err != nil {
		return nil, err
	}

	o := m.Origin()
	shown := &shownSource{
		Source: address, Version: o.Version, Metadata: m.Metadata(),
		Repository: o.Repository, Ref: o.Ref, Commit: o.Commit, Digest: o.Digest,
		Signers: nonNil(o.Signers), Warnings: nonNil(o.Warnings),
		Variables: m.Variables(), Outputs: m.Outputs(),
	}

	if req, err := source.Parse(address, nil); err == nil && req.Kind == source.KindBuiltin {
		shown.Builtin = true
	}

	if f.readme {
		readme, err := m.Readme()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}

		shown.Readme = string(readme)
	}

	return shown, nil
}

// showStack fetches the newest version of a published stack that meets the
// constraint, with the modules its lock file pins, as the stack commands
// fetch one.
func showStack(ctx context.Context, eng *rigger.Engine, stderr io.Writer, address string, f showFlags) (*shownSource, error) {
	s, diags := eng.FetchStack(ctx, address, f.version)
	if diags.HasErrors() {
		return nil, reportDiags(stderr, address, diags)
	}

	o := s.Origin()
	shown := &shownSource{
		Source: address, Version: o.Version,
		Repository: o.Repository, Ref: o.Ref, Commit: o.Commit, Digest: o.Digest,
		Signers: nonNil(o.Signers), Warnings: []string{},
		Variables: s.Variables(), Outputs: s.Outputs(), Modules: s.Modules(),
	}

	if md := s.Metadata(); md != nil {
		shown.Metadata = *md
	}

	for _, d := range diags {
		if d.Severity == hcl.DiagWarning {
			shown.Warnings = append(shown.Warnings, strings.TrimSpace(d.Summary+": "+d.Detail))
		}
	}

	// A stack nothing signed is taken as a module is, with a warning.
	if len(o.Signers) == 0 {
		detail := fmt.Sprintf("%s %s is not signed: only its commit and tree digest are checked.", o.Source, o.Version)
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: "Stack not signed", Detail: detail})
		shown.Warnings = append(shown.Warnings, strings.TrimSuffix(detail, "."))
	}

	if err := writeWarnings(stderr, address, warningsOnly(diags)); err != nil {
		return nil, err
	}

	if f.readme {
		readme, err := s.Readme()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}

		shown.Readme = string(readme)
	}

	return shown, nil
}

// warningsOnly returns the warnings of diags.
func warningsOnly(diags hcl.Diagnostics) hcl.Diagnostics {
	var out hcl.Diagnostics

	for _, d := range diags {
		if d.Severity == hcl.DiagWarning {
			out = append(out, d)
		}
	}

	return out
}

// nonNil is s, or an empty list for nil: JSON says [] rather than null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}

	return s
}

// writeDetails writes a module or a stack for a person, or as JSON.
func writeDetails(w io.Writer, s *shownSource, f showFlags) error {
	if f.json {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)

		return enc.Encode(s)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%s %s %s\n", s.Kind, s.Source, s.Version)

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	md := s.Metadata

	for _, row := range [][2]string{
		{"description", md.Description},
		{"url", md.URL},
		{"license", md.License},
		{"category", md.Category},
		{"keywords", strings.Join(md.Keywords, ", ")},
		{"requires", requiresText(md.Requires)},
		{"versions", strings.Join(s.Versions, " ")},
	} {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", row[0], orDash(row[1]))
	}

	for _, row := range [][2]string{{"repository", s.Repository}, {"tag", s.Ref}, {"commit", s.Commit}} {
		if row[1] != "" {
			_, _ = fmt.Fprintf(tw, "  %s\t%s\n", row[0], source.Redact(row[1]))
		}
	}

	_, _ = fmt.Fprintf(tw, "  digest\t%s\n", orDash(s.Digest))
	_, _ = fmt.Fprintf(tw, "  signed by\t%s\n", signersText(s))

	if err := tw.Flush(); err != nil {
		return err
	}

	writeVariables(&b, s.Variables)
	writeOutputs(&b, s.Outputs)

	if s.Kind == index.KindStack {
		writeStackModules(&b, s.Modules)
	}

	if f.readme {
		if s.Readme == "" {
			b.WriteString("\nREADME.md: none\n")
		} else {
			fmt.Fprintf(&b, "\nREADME.md:\n%s", s.Readme)

			if !strings.HasSuffix(s.Readme, "\n") {
				b.WriteString("\n")
			}
		}
	}

	_, err := io.WriteString(w, b.String())

	return err
}

// signersText is who signed a version: the key IDs, else why none did.
func signersText(s *shownSource) string {
	switch {
	case len(s.Signers) > 0:
		return strings.Join(s.Signers, " ")
	case s.Builtin:
		return "not signed: built into the program"
	default:
		return "not signed"
	}
}

// requiresText is the requires block of metadata on one line.
func requiresText(r module.Requires) string {
	var parts []string

	if r.Norca != "" {
		parts = append(parts, "norca "+r.Norca)
	}

	if len(r.Features) > 0 {
		parts = append(parts, "features "+strings.Join(r.Features, ", "))
	}

	for _, m := range r.Modules {
		text := "module " + m.Name
		if m.Source != "" && m.Source != m.Name {
			text += " (" + m.Source + ")"
		}

		if m.Version != "" {
			text += " " + m.Version
		}

		if m.When != "" {
			text += " when " + m.When
		}

		parts = append(parts, text)
	}

	return strings.Join(parts, "; ")
}

// writeVariables writes the variables as a table.
func writeVariables(b *strings.Builder, vars []module.Variable) {
	if len(vars) == 0 {
		b.WriteString("\nvariables: none\n")

		return
	}

	b.WriteString("\nvariables:\n")

	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  NAME\tTYPE\tDEFAULT\tREQUIRED\tSENSITIVE\tDESCRIPTION")

	for _, v := range vars {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", v.Name, orDash(v.Type), orDash(v.Default), yesNo(v.Required), yesNo(v.Sensitive), orDash(oneLine(v.Description)))
	}

	_ = tw.Flush() // a strings.Builder does not fail
}

// writeOutputs writes the outputs as a table.
func writeOutputs(b *strings.Builder, outputs []module.Output) {
	if len(outputs) == 0 {
		b.WriteString("\noutputs: none\n")

		return
	}

	b.WriteString("\noutputs:\n")

	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  NAME\tSENSITIVE\tDESCRIPTION")

	for _, o := range outputs {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", o.Name, yesNo(o.Sensitive), orDash(oneLine(o.Description)))
	}

	_ = tw.Flush() // a strings.Builder does not fail
}

// writeStackModules writes the module blocks of a stack as a table.
func writeStackModules(b *strings.Builder, mods []rigger.StackModule) {
	b.WriteString("\nmodules:\n")

	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  RELEASE\tSOURCE\tCONSTRAINT\tVERSION")

	for _, m := range mods {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", m.Release, source.Redact(m.Source), orDash(m.Constraint), orDash(m.Version))
	}

	_ = tw.Flush() // a strings.Builder does not fail
}

// oneLine is a description on one line of a table.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}
