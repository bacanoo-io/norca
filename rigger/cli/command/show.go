// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/hashicorp/hcl/v2"
	"github.com/spf13/cobra"

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
	kind     string
	version  string
	readme   bool
	json     bool
	maxWidth int
}

// newShowCommand is rigger show <source> [flags].
func (a *app) newShowCommand() *cobra.Command {
	var f showFlags

	cmd := &cobra.Command{
		Use:   "show <source>",
		Short: "Show one module or stack, without a cluster",
		Long: `Show one module or stack without a cluster: the newest version that meets
--version, fetched and verified as an install does. It shows the metadata,
the versions, the digest, the signers, the variables, the outputs and, of a
stack, its modules; --readme adds the README.

The source is <namespace>/<name> of the global registry, builtin/<name>, a
git address (a module of a registry repository:
<repository>//modules/<name>) or oci://<host>/<repository>.`,
		Example: `  rigger show norca/redis
  rigger show norca/nats --version "~> 1.2" --readme
  rigger show builtin/redis --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.maxWidth < 0 {
				return errMaxWidth
			}

			if len(args) != 1 {
				return errShowSource
			}

			if f.kind != "" && f.kind != string(index.KindModule) && f.kind != string(index.KindStack) {
				return fmt.Errorf("%w, not %q", errShowKind, f.kind)
			}

			eng, err := offlineEngine(a.logger)
			if err != nil {
				return err
			}

			shown, err := showSource(cmd.Context(), eng, a.stderr, args[0], f)
			if err != nil {
				return err
			}

			return writeDetails(a.stdout, shown, f)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&f.kind, "kind", "", "module or stack (default: a stack when the address names one, as stacks/<name> of a repository or a stack of the global registry)")
	flags.StringVar(&f.version, versionFlag, "", `the version `+"`constraint`"+` ("~> 1.2"); none shows the newest`)
	flags.BoolVar(&f.readme, "readme", false, "print the README.md too")
	flags.BoolVar(&f.json, jsonFlag, false, "write the details as JSON")
	flags.IntVar(&f.maxWidth, maxWidthFlag, defaultMaxWidth, maxWidthUsage)

	return cmd
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

	writeVariables(&b, s.Variables, f.maxWidth)
	writeOutputs(&b, s.Outputs, f.maxWidth)

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

// writeVariables writes the variables as a table; a default and a
// description longer than maxWidth are cut (fitText).
func writeVariables(b *strings.Builder, vars []module.Variable, maxWidth int) {
	if len(vars) == 0 {
		b.WriteString("\nvariables: none\n")

		return
	}

	b.WriteString("\nvariables:\n")

	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  NAME\tTYPE\tDEFAULT\tREQUIRED\tSENSITIVE\tDESCRIPTION")

	for _, v := range vars {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", v.Name, orDash(v.Type), orDash(fitText(v.Default, maxWidth)), yesNo(v.Required), yesNo(v.Sensitive),
			orDash(fitText(v.Description, maxWidth)))
	}

	_ = tw.Flush() // a strings.Builder does not fail
}

// writeOutputs writes the outputs as a table; a description longer than
// maxWidth is cut (fitText).
func writeOutputs(b *strings.Builder, outputs []module.Output, maxWidth int) {
	if len(outputs) == 0 {
		b.WriteString("\noutputs: none\n")

		return
	}

	b.WriteString("\noutputs:\n")

	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  NAME\tSENSITIVE\tDESCRIPTION")

	for _, o := range outputs {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", o.Name, yesNo(o.Sensitive), orDash(fitText(o.Description, maxWidth)))
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

// defaultMaxWidth is how many characters of a description a table shows, so
// that a row of rigger search or rigger show fits a line of a terminal.
const defaultMaxWidth = 60

// maxWidthFlag is the flag of the width of a description in a table.
const maxWidthFlag = "max-width"

const maxWidthUsage = "the characters of a description (and of a default) a table shows; 0 shows them whole"

var errMaxWidth = errors.New("-max-width is 0 (no limit) or more")

// ellipsis marks a text fitText cut.
const ellipsis = "..."

// fitText is a text on one line of at most maxWidth characters (0: no
// limit). A longer one is cut at the last space of its second half, or in
// the word when there is none, and ends with "...".
func fitText(s string, maxWidth int) string {
	s = oneLine(s)

	r := []rune(s)
	if maxWidth <= 0 || len(r) <= maxWidth {
		return s
	}

	if maxWidth <= len(ellipsis) {
		return string(r[:maxWidth])
	}

	cut := maxWidth - len(ellipsis)
	if r[cut] != ' ' {
		if i := lastSpace(r[:cut]); i > cut/2 {
			cut = i
		}
	}

	return strings.TrimRight(string(r[:cut]), " ,;:") + ellipsis
}

// lastSpace is the index of the last space of r; -1 when it has none.
func lastSpace(r []rune) int {
	for i, c := range slices.Backward(r) {
		if c == ' ' {
			return i
		}
	}

	return -1
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}
