// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/nomad/pack/internal/pkg/flag"
	"github.com/posener/complete"
)

type FmtCommand struct {
	*baseCommand
	check     bool
	list      bool
	write     bool
	recursive bool
}

func (c *FmtCommand) Run(args []string) int {
	c.cmdKey = "fmt"

	if err := c.Init(
		WithArgs(args),
		WithFlags(c.Flags()),
		WithNoConfig(),
	); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}

	paths := c.args
	if len(paths) == 0 {
		paths = []string{"."}
	}

	return c.fmt(paths)
}

func (c *FmtCommand) fmt(paths []string) int {
	var exitCode int
	var filesToFormat []string

	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			c.ui.Error(fmt.Sprintf("Error accessing %s: %v", path, err))
			return 1
		}

		if info.IsDir() {
			files, err := c.findFormattableFiles(path)
			if err != nil {
				c.ui.Error(fmt.Sprintf("Error scanning directory: %v", err))
				return 1
			}
			filesToFormat = append(filesToFormat, files...)
		} else if c.isFormattableFile(path) {
			filesToFormat = append(filesToFormat, path)
		}
	}

	if len(filesToFormat) == 0 {
		c.ui.Info("No formattable files (.tpl,.hcl) found")
		return 0
	}

	for _, file := range filesToFormat {
		formatted, err := c.formatFile(file)
		var notHCL *notHCLTemplateError
		if errors.As(err, &notHCL) {
			// text templates are left as they are
			c.ui.Info(fmt.Sprintf("Skipped %s: %v", file, err))
			continue
		}
		if err != nil {
			c.ui.Error(fmt.Sprintf("Error formatting %s: %v", file, err))
			exitCode = 1
			continue
		}

		if formatted {
			if !c.write && c.list {
				c.ui.Output(file)
			}
			if c.check {
				exitCode = 1
			}
		}
	}

	if c.check && exitCode == 1 {
		c.ui.Error("Some files are not formatted. Run 'norca pack fmt' to format them.")
	}

	return exitCode
}

func (c *FmtCommand) formatFile(path string) (bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var formatted string
	switch {
	case strings.HasSuffix(path, ".tpl"):
		if !templateRendersHCL(path) {
			return false, notHCLTemplate("it renders a %s file", filepath.Ext(strings.TrimSuffix(path, ".tpl")))
		}
		formatted, err = c.formatTemplate(string(content))
	case strings.HasSuffix(path, ".hcl"):
		formatted, err = c.formatHCL(string(content))
	default:
		return false, fmt.Errorf("unsupported file type: %s", path)
	}
	if err != nil {
		return false, err
	}
	changed := string(content) != formatted

	if changed && c.write && !c.check {
		err = os.WriteFile(path, []byte(formatted), 0644)
		if err != nil {
			return false, err
		}
		c.ui.Output(fmt.Sprintf("Formatted:%s", path))
	}
	return changed, nil
}

// notHCLTemplateError is returned for a template that is not an HCL template
// or would not survive HCL formatting: it is left unchanged.
type notHCLTemplateError struct {
	reason string
}

func (e *notHCLTemplateError) Error() string {
	return "not an HCL template: " + e.reason
}

func notHCLTemplate(format string, args ...any) error {
	return &notHCLTemplateError{reason: fmt.Sprintf(format, args...)}
}

var (
	// templateActionRe matches a template action, including multi-line
	// comments.
	templateActionRe = regexp.MustCompile(`(?s)\[\[.*?\]\]`)

	// defineActionRe matches a define action.
	defineActionRe = regexp.MustCompile(`^\[\[-?\s*define\s`)

	// actionTokenRe matches the stand-in of an action while lines are
	// classified.
	actionTokenRe = regexp.MustCompile("\x00([0-9]+)\x00")

	// actionLineRe matches a line made of action stand-ins only.
	actionLineRe = regexp.MustCompile("^(\x00[0-9]+\x00[ \t]*)+$")

	// lineTokenRe matches the comment that stands in for a line of actions
	// during formatting.
	lineTokenRe = regexp.MustCompile(`#[ \t]*` + tplLinePrefix + `([0-9]+)__`)
)

const (
	tplInlinePrefix = "__NORCA_PACK_TPL_"
	tplLinePrefix   = "__NORCA_PACK_TPL_LINE_"
)

// formatTemplate formats a pack template as HCL. Only HCL templates are
// formatted: the template must be HCL once its actions are replaced, a line
// holding only actions by a comment and an action within a line by an
// identifier. It returns a notHCLTemplateError, and no change, for text
// templates (such as outputs.tpl), helper files with define blocks (their
// fragments are indented for the place they are included) and templates
// where formatting would change the text around an action.
func (c *FmtCommand) formatTemplate(content string) (string, error) {
	// replace the actions by stand-ins, so lines can be classified
	var actions []string
	marked := templateActionRe.ReplaceAllStringFunc(content, func(action string) string {
		actions = append(actions, action)
		return fmt.Sprintf("\x00%d\x00", len(actions)-1)
	})
	for _, action := range actions {
		if defineActionRe.MatchString(action) {
			return "", notHCLTemplate("it defines template fragments")
		}
	}

	// a line of actions only becomes a comment, an action within a line an
	// identifier
	var lineActions []string
	inline := make(map[string]string)
	lines := strings.Split(marked, "\n")
	for i, line := range lines {
		body := strings.TrimRight(line, "\r")
		cr := line[len(body):]
		trimmed := strings.TrimSpace(body)
		if trimmed != "" && actionLineRe.MatchString(trimmed) {
			indent := body[:len(body)-len(strings.TrimLeft(body, " \t"))]
			lines[i] = fmt.Sprintf("%s# %s%d__%s", indent, tplLinePrefix, len(lineActions), cr)
			lineActions = append(lineActions, trimmed)
			continue
		}
		lines[i] = actionTokenRe.ReplaceAllStringFunc(line, func(token string) string {
			id := fmt.Sprintf("%s%s__", tplInlinePrefix, strings.Trim(token, "\x00"))
			inline[id] = token
			return id
		})
	}
	src := strings.Join(lines, "\n")

	if err := checkHCLTemplate(src, inline); err != nil {
		return "", err
	}
	formatted := string(hclwrite.Format([]byte(src)))
	if _, diags := hclsyntax.ParseConfig([]byte(formatted), "template", hcl.InitialPos); diags.HasErrors() {
		return "", notHCLTemplate("formatting would break it: %s", diags.Error())
	}
	for id := range inline {
		if touching(src, id) != touching(formatted, id) {
			return "", notHCLTemplate("formatting would change the text around a template action")
		}
	}

	// restore the actions
	formatted = lineTokenRe.ReplaceAllStringFunc(formatted, func(token string) string {
		n, _ := strconv.Atoi(lineTokenRe.FindStringSubmatch(token)[1])
		return lineActions[n]
	})
	for id, token := range inline {
		formatted = strings.ReplaceAll(formatted, id, token)
	}
	restored := actionTokenRe.ReplaceAllStringFunc(formatted, func(token string) string {
		n, _ := strconv.Atoi(strings.Trim(token, "\x00"))
		return actions[n]
	})
	return restored, nil
}

// checkHCLTemplate checks that src, a template with its actions replaced, is
// HCL, and that every inline action stands for a whole HCL token or is part
// of a string or a comment.
func checkHCLTemplate(src string, inline map[string]string) error {
	if _, diags := hclsyntax.ParseConfig([]byte(src), "template", hcl.InitialPos); diags.HasErrors() {
		return notHCLTemplate("%s", diags.Errs()[0].Error())
	}
	tokens, _ := hclsyntax.LexConfig([]byte(src), "template", hcl.InitialPos)
	for _, tok := range tokens {
		if !strings.Contains(string(tok.Bytes), tplInlinePrefix) {
			continue
		}
		switch tok.Type {
		case hclsyntax.TokenQuotedLit, hclsyntax.TokenStringLit, hclsyntax.TokenComment:
			continue
		case hclsyntax.TokenIdent:
			if _, ok := inline[string(tok.Bytes)]; ok {
				continue
			}
		}
		return notHCLTemplate("a template action is joined to HCL text at line %d", tok.Range.Start.Line)
	}
	return nil
}

// touching returns whether the first occurrence of id in s has no space before
// and after it.
func touching(s, id string) [2]bool {
	i := strings.Index(s, id)
	if i < 0 {
		return [2]bool{}
	}
	isSpace := func(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
	before := i > 0 && !isSpace(s[i-1])
	end := i + len(id)
	after := end < len(s) && !isSpace(s[end])
	return [2]bool{before, after}
}

// formatHCL formats an HCL file; it fails on syntax errors.
func (c *FmtCommand) formatHCL(content string) (string, error) {
	if _, diags := hclsyntax.ParseConfig([]byte(content), "file", hcl.InitialPos); diags.HasErrors() {
		return "", diags
	}
	formatted := hclwrite.Format([]byte(content))
	return string(formatted), nil
}

// templateRendersHCL returns whether a template file renders an HCL file
// judging by its name: x.nomad.tpl, x.hcl.tpl and x.tpl do, x.json.tpl or
// x.yaml.tpl do not.
func templateRendersHCL(path string) bool {
	inner := filepath.Ext(strings.TrimSuffix(filepath.Base(path), ".tpl"))
	switch inner {
	case "", ".nomad", ".hcl":
		return true
	}
	return false
}

func (c *FmtCommand) findFormattableFiles(dir string) ([]string, error) {
	var files []string
	cleanDir := filepath.Clean(dir)
	walkFunc := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() && path != cleanDir && !c.recursive {
			return filepath.SkipDir
		}
		if !d.IsDir() && c.isFormattableFile(path) {
			files = append(files, path)
		}

		return nil
	}

	err := filepath.WalkDir(cleanDir, walkFunc)
	return files, err
}

func (c *FmtCommand) isFormattableFile(path string) bool {
	return strings.HasSuffix(path, ".tpl") ||
		strings.HasSuffix(path, ".hcl")
}

func (c *FmtCommand) Flags() *flag.Sets {
	return c.flagSet(0, func(set *flag.Sets) {
		f := set.NewSet("Format Options")

		f.BoolVar(&flag.BoolVar{
			Name:    "check",
			Target:  &c.check,
			Default: false,
			Usage:   "Check if files are formatted without modifying them. Returns exit code 1 if formatting is needed.",
		})

		f.BoolVar(&flag.BoolVar{
			Name:    "list",
			Target:  &c.list,
			Default: true,
			Usage:   "List files that would be modified.",
		})

		f.BoolVar(&flag.BoolVar{
			Name:    "write",
			Target:  &c.write,
			Default: true,
			Usage:   "Write formatted content back to files.",
		})

		f.BoolVar(&flag.BoolVar{
			Name:    "recursive",
			Target:  &c.recursive,
			Default: false,
			Usage:   "Process directories recursively.",
		})
	})
}

func (c *FmtCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictOr(
		complete.PredictFiles("*.tpl"),
		complete.PredictFiles("*.hcl"),
	)
}

func (c *FmtCommand) AutocompleteFlags() complete.Flags {
	return c.Flags().Completions()
}
func (c *FmtCommand) Synopsis() string {
	return "Format pack template and HCL files"
}

func (c *FmtCommand) Help() string {
	return formatHelp(`
Usage: norca pack fmt [options] [path...]

  Format pack files (.tpl templates and .hcl files) using HCL formatting rules.
  Template files preserve the [[ ]] syntax, while .hcl files are formatted directly.

  Only HCL templates are formatted. A template is skipped, with a note, when it
  is not HCL once its [[ ]] actions are set aside (a text template such as
  outputs.tpl), when it renders another file type (x.json.tpl), when it
  defines template fragments ([[ define ]], as in _helpers.tpl), or when
  formatting would change the text around an action. Skipped files do not
  fail -check. An .hcl file with syntax errors is an error.

  If no path is given, the current directory is used.

Format Options:

  -check
    Check if files are formatted without modifying them.
    Returns exit code 1 if formatting is needed.

  -list
    List files that would be modified (default: true).

  -write
    Write formatted content back to files (default: true).

  -recursive
    Process directories recursively.

Examples:

  Format all templates in current directory:
  $ "norca pack fmt"

  Format a specific pack directory:
  $ "norca pack fmt my-pack/"

  Check formatting without modifying:
  $ "norca pack fmt -check ."
`)
}
