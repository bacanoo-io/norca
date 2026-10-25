// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
)

func runNotices(args []string) error {
	fs := flag.NewFlagSet("notices", flag.ExitOnError)
	var c commonFlags
	c.register(fs)
	out := fs.String("out", "", "write the notices to this file (default: standard output)")
	check := fs.String("check", "", "compare the notices with this file and fail if it is out of date")
	_ = fs.Parse(args)

	inv, err := collect(c)
	if err != nil {
		return err
	}
	doc := renderNotices(inv)

	switch {
	case *check != "":
		old, err := os.ReadFile(*check)
		if err != nil {
			return err
		}
		// Compare with LF line endings: git may check the file out with
		// CRLF on Windows.
		if !bytes.Equal(bytes.ReplaceAll(old, []byte("\r\n"), []byte("\n")), doc) {
			return fmt.Errorf("%s is out of date: run 'make notices' in nomad/ and commit the result", *check)
		}
		fmt.Fprintf(os.Stderr, "%s is up to date\n", *check)
	case *out != "":
		if err := os.WriteFile(*out, doc, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d Go modules, %d npm packages)\n", *out, len(inv.goMods), len(inv.npm))
	default:
		_, err = os.Stdout.Write(doc)
		return err
	}
	return nil
}

// inventory is the set of third-party components of the shipped binaries.
type inventory struct {
	goMods map[string]*goModule
	npm    []*npmPackage
	// legacyUI reports whether the npm list includes the previous UI.
	legacyUI bool
}

// collect builds the inventory and fails if a license is not allowed.
func collect(c commonFlags) (*inventory, error) {
	mods, err := collectGoModules(c.root)
	if err != nil {
		return nil, err
	}
	problems, err := resolveGoLicenses(c.root, mods)
	if err != nil {
		return nil, err
	}
	inv := &inventory{goMods: mods}
	if !c.noUI {
		pkgs, p, err := collectUIs(c)
		if err != nil {
			return nil, err
		}
		inv.npm = pkgs
		inv.legacyUI = legacyUIPresent(c.root)
		problems = append(problems, p...)
	}
	if len(problems) > 0 {
		return nil, licenseError(problems)
	}
	if err := checkOverrides(c.strict, true, c.kinds()...); err != nil {
		return nil, err
	}
	return inv, nil
}

// legacyUINote names the manifests of the previous UI when it is embedded.
func legacyUINote(inv *inventory) string {
	if !inv.legacyUI {
		return ""
	}
	return " and of the previous UI served at `/ui/legacy` for one release (`nomad/ui-legacy/package.json`, `nomad/ui-legacy/yarn.lock`)"
}

func licenseError(problems []string) error {
	return errors.New("license check failed:\n  " + strings.Join(problems, "\n  ") +
		"\nOnly the licenses allowed by docs/licensing.md may be shipped; record a reviewed decision in " +
		"nomad/tools/thirdparty/overrides.go only if the component is offered under an allowed license.")
}

// textIndex numbers the distinct license texts. Texts that differ only in
// whitespace and punctuation are printed once.
type textIndex struct {
	byKey map[string]int
	texts []string
	users [][]string
}

func (t *textIndex) add(text, user string) int {
	k := canonical(text)
	n, ok := t.byKey[k]
	if !ok {
		t.texts = append(t.texts, text)
		t.users = append(t.users, nil)
		n = len(t.texts)
		t.byKey[k] = n
	}
	if u := t.users[n-1]; len(u) == 0 || u[len(u)-1] != user {
		t.users[n-1] = append(u, user)
	}
	return n
}

var backticks = regexp.MustCompile("`+")

// fence returns a Markdown code fence longer than any backtick run in text.
func fence(text string) string {
	n := 3
	for _, m := range backticks.FindAllString(text, -1) {
		if len(m) >= n {
			n = len(m) + 1
		}
	}
	return strings.Repeat("`", n)
}

func writeBlock(b *bytes.Buffer, text string) {
	f := fence(text)
	fmt.Fprintf(b, "%stext\n%s\n%s\n", f, text, f)
}

// mdEscape escapes the characters that would break a table cell or inline
// text.
func mdEscape(s string) string {
	r := strings.NewReplacer("|", `\|`, "<", "&lt;", ">", "&gt;", "*", `\*`, "_", `\_`, "`", "\\`", "[", `\[`, "]", `\]`)
	return r.Replace(s)
}

func goName(m *goModule) string {
	if m.Path == stdlib {
		return "Go standard library"
	}
	return m.Path
}

func goVersion(m *goModule) string {
	if m.Path == stdlib {
		return ""
	}
	if m.ReplacePath != "" {
		return fmt.Sprintf("%s (replaced by %s %s)", m.Version, m.ReplacePath, m.ReplaceVersion)
	}
	return m.Version
}

func usedIn(set map[string]bool) string {
	var out []string
	for _, b := range binaries {
		if set[b.Name] {
			out = append(out, b.Name)
		}
	}
	return strings.Join(out, ", ")
}

func uiBinaries() string {
	var out []string
	for _, b := range binaries {
		if b.UI {
			out = append(out, b.Name)
		}
	}
	return strings.Join(out, ", ") + " (web UI)"
}

func renderNotices(inv *inventory) []byte {
	var b bytes.Buffer
	idx := &textIndex{byKey: map[string]int{}}
	keys := sortedModKeys(inv.goMods)

	var names []string
	for _, bn := range binaries {
		names = append(names, "`"+bn.Name+"`")
	}
	var plats []string
	for _, p := range platforms {
		plats = append(plats, p.GOOS+"/"+p.GOARCH)
	}

	b.WriteString("<!-- Generated by nomad/tools/thirdparty (run 'make notices' in nomad/). Do not edit. -->\n\n")
	b.WriteString("# Third-party notices\n\n")
	b.WriteString("Norca is licensed under the Mozilla Public License 2.0; see `LICENSE` and `NOTICE`. ")
	fmt.Fprintf(&b, "The %s binaries and the container images also contain the third-party components listed below, each under its own license. ", strings.Join(names, " and "))
	b.WriteString("The license files of each component are reproduced in full under \"License texts\". ")
	b.WriteString("Where a component ships NOTICE files, their contents are reproduced with it, as section 4(d) of the Apache License 2.0 requires.\n\n")
	fmt.Fprintf(&b, "The list covers the Go modules linked into the binaries on every release platform (%s) and the production npm dependencies of the web UI embedded in `norca` (`nomad/ui/package.json`, `nomad/ui/yarn.lock`)%s.\n\n", strings.Join(plats, ", "), legacyUINote(inv))

	// Summary.
	b.WriteString("## Summary\n\n")
	b.WriteString("| Component | Version | License | Used in |\n| --- | --- | --- | --- |\n")
	for _, k := range keys {
		m := inv.goMods[k]
		fmt.Fprintf(&b, "| [%s](#%s) | %s | %s | %s |\n", mdEscape(goName(m)), goAnchor(m), mdEscape(goVersion(m)), m.Expr, usedIn(m.UsedIn))
	}
	for _, p := range inv.npm {
		fmt.Fprintf(&b, "| [%s](#%s) (npm) | %s | %s | %s |\n", mdEscape(p.Name), npmAnchor(p), p.Version, mdEscape(p.Expr), uiBinaries())
	}

	// Go modules.
	b.WriteString("\n## Go modules\n")
	for _, k := range keys {
		m := inv.goMods[k]
		fmt.Fprintf(&b, "\n<a id=\"%s\"></a>\n\n### %s\n\n", goAnchor(m), strings.TrimSpace(mdEscape(goName(m))+" "+mdEscape(goVersion(m))))
		fmt.Fprintf(&b, "- License: %s\n", m.Expr)
		if m.Override != nil {
			fmt.Fprintf(&b, "- License decision: %s\n", m.Override.Reason)
		}
		fmt.Fprintf(&b, "- Used in: %s\n", usedIn(m.UsedIn))
		writeTextRefs(&b, idx, goName(m)+" "+m.Version, m.Licenses)
		writeNotices(&b, m.Notices)
	}

	// npm packages.
	if len(inv.npm) > 0 {
		b.WriteString("\n## npm packages of the web UI\n")
		for _, p := range inv.npm {
			fmt.Fprintf(&b, "\n<a id=\"%s\"></a>\n\n### %s %s\n\n", npmAnchor(p), mdEscape(p.Name), p.Version)
			fmt.Fprintf(&b, "- License: %s", mdEscape(p.Expr))
			if p.Declared != "" && p.Declared != p.Expr {
				fmt.Fprintf(&b, " (declared: %s)", mdEscape(p.Declared))
			}
			b.WriteString("\n")
			if p.Override != nil {
				fmt.Fprintf(&b, "- License decision: %s\n", p.Override.Reason)
			}
			fmt.Fprintf(&b, "- Required through: %s\n", mdEscape(p.Via))
			if p.Author != "" {
				fmt.Fprintf(&b, "- Author: %s\n", mdEscape(p.Author))
			}
			if len(p.Licenses) == 0 {
				b.WriteString("- The package contains no license file; the license is the one declared in its package.json.\n")
			}
			writeTextRefs(&b, idx, p.Name+" "+p.Version, p.Licenses)
			writeNotices(&b, p.Notices)
		}
	}

	// License texts.
	b.WriteString("\n## License texts\n")
	for i, t := range idx.texts {
		fmt.Fprintf(&b, "\n<a id=\"license-text-%d\"></a>\n\n### License text %d\n\n", i+1, i+1)
		var us []string
		for _, u := range idx.users[i] {
			us = append(us, mdEscape(u))
		}
		fmt.Fprintf(&b, "Used by: %s\n\n", strings.Join(us, ", "))
		writeBlock(&b, t)
	}
	return b.Bytes()
}

func writeTextRefs(b *bytes.Buffer, idx *textIndex, user string, files []textFile) {
	for _, f := range files {
		if f.Text == "" {
			continue
		}
		n := idx.add(f.Text, user)
		fmt.Fprintf(b, "- `%s`: [license text %d](#license-text-%d)\n", f.Path, n, n)
	}
}

func writeNotices(b *bytes.Buffer, files []textFile) {
	for _, f := range files {
		if f.Text == "" {
			continue
		}
		fmt.Fprintf(b, "\nNOTICE (`%s`):\n\n", f.Path)
		writeBlock(b, f.Text)
	}
}

var anchorRe = regexp.MustCompile(`[^a-z0-9]+`)

func anchor(prefix, s string) string {
	return prefix + "-" + strings.Trim(anchorRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func goAnchor(m *goModule) string {
	if m.Path == stdlib {
		return "go-stdlib"
	}
	return anchor("go", m.Path+" "+m.Version)
}

func npmAnchor(p *npmPackage) string { return anchor("npm", p.Name+" "+p.Version) }
