// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"mit", `Copyright (c) 2020 A

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction... The above copyright notice and this
permission notice shall be included in all copies or substantial portions.`, []string{"MIT"}},
		{"bsd3", `Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:
* Neither the name of Google Inc. nor the names of its contributors may be used`, []string{"BSD-3-Clause"}},
		{"bsd2", `Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:
1. Redistributions of source code must retain the above copyright notice.`, []string{"BSD-2-Clause"}},
		{"isc", `Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.`, []string{"ISC"}},
		{"apache", "Apache License\nVersion 2.0, January 2004\nTERMS AND CONDITIONS", []string{"Apache-2.0"}},
		{"mpl", "Mozilla Public License, version 2.0\n\n1. Definitions", []string{"MPL-2.0"}},
		// The MPL names the GNU licenses as Secondary Licenses; that alone
		// is not a GNU license.
		{"mpl-secondary", "Mozilla Public License Version 2.0\nSecondary License means either the GNU General Public License, Version 2.0, the GNU Lesser General Public License", []string{"MPL-2.0"}},
		{"gpl", "GNU GENERAL PUBLIC LICENSE\nEveryone is permitted to copy and distribute verbatim copies of this license document", []string{"GPL-family"}},
		{"busl", "Business Source License 1.1\nParameters", []string{"BUSL-1.1"}},
		{"dual", "MIT section: Permission is hereby granted, free of charge, to any person obtaining a copy ... The above copyright notice and this permission notice shall be included\n\nApache License\nVersion 2.0", []string{"Apache-2.0", "MIT"}},
		{"unknown", "All rights reserved.", nil},
	}
	for _, c := range cases {
		got := licenseIDs([]textFile{{Text: c.text}})
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestExprAllowed(t *testing.T) {
	allowed := setOf("MIT", "Apache-2.0")
	for expr, want := range map[string]bool{
		"MIT":                                  true,
		"(MIT OR GPL-3.0)":                     true,
		"MIT AND GPL-3.0":                      false,
		"(MIT AND Apache-2.0)":                 true,
		"GPL-2.0 WITH Classpath-exception-2.0": false,
		"BSD":                                  false,
		"MIT OR":                               false,
		"":                                     false,
	} {
		if got := exprAllowed(expr, allowed); got != want {
			t.Errorf("exprAllowed(%q) = %v, want %v", expr, got, want)
		}
	}
}

func TestParseYarnLock(t *testing.T) {
	lock := `# yarn lockfile v1


"@scope/a@^1.0.0", "@scope/a@^1.1.0":
  version "1.2.0"
  resolved "https://registry.yarnpkg.com/@scope/a/-/a-1.2.0.tgz#abc"
  integrity sha512-AAAA
  dependencies:
    b "~2.0.0"
  optionalDependencies:
    "c" "^3.0.0"

b@~2.0.0:
  version "2.0.1"
`
	m, err := parseYarnLock([]byte(lock))
	if err != nil {
		t.Fatal(err)
	}
	a := m["@scope/a@^1.1.0"]
	if a == nil || a != m["@scope/a@^1.0.0"] {
		t.Fatalf("entry for @scope/a not shared: %v", m)
	}
	if a.Version != "1.2.0" || !strings.HasSuffix(a.Resolved, "#abc") || a.Integrity != "sha512-AAAA" {
		t.Errorf("unexpected entry %+v", a)
	}
	if want := map[string]string{"b": "~2.0.0", "c": "^3.0.0"}; !reflect.DeepEqual(a.Dependencies, want) {
		t.Errorf("dependencies = %v, want %v", a.Dependencies, want)
	}
	if m["b@~2.0.0"].Version != "2.0.1" {
		t.Errorf("b: %+v", m["b@~2.0.0"])
	}
}

func TestNormalizeText(t *testing.T) {
	bom, cr, ff := string(rune(0xFEFF)), string(rune(13)), string(rune(12))
	in := bom + cr + "\n" + cr + "\nline one  " + cr + "\nline" + ff + "two\t" + cr + "\n" + cr + "\n"
	if got, want := normalizeText(in), "line one\nlinetwo"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFence(t *testing.T) {
	if got := fence("a ``` b"); got != "````" {
		t.Errorf("got %q", got)
	}
	if got := fence("plain"); got != "```" {
		t.Errorf("got %q", got)
	}
}

func TestFindSourceCopy(t *testing.T) {
	for path, want := range map[string]string{
		"github.com/hashicorp/nomad/secretstore/ubix":                  "github.com/cwolsen7905/ubixvault",
		"github.com/hashicorp/nomad/secretstore/ubix/internal/barrier": "github.com/cwolsen7905/ubixvault",
		"github.com/hashicorp/nomad/secretstore/ubix/export/core":      "github.com/cwolsen7905/ubixvault",
		"github.com/hashicorp/nomad/secretstore":                       "",
		"github.com/hashicorp/nomad/secretstore/ubixother":             "",
		"github.com/hashicorp/nomad/nomad":                             "",
	} {
		got := ""
		if c := findSourceCopy(path); c != nil {
			got = c.Path
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}

// writeUI lays out a UI directory with one installed dependency.
func writeUI(t *testing.T, name, license string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"package.json": `{"dependencies": {"` + name + `": "^1.0.0"}}`,
		"yarn.lock": "# yarn lockfile v1\n\n" + name + "@^1.0.0:\n  version \"1.0.1\"\n" +
			"  resolved \"https://registry.yarnpkg.com/" + name + "/-/" + name + "-1.0.1.tgz#x\"\n",
		"node_modules/" + name + "/package.json": `{"name": "` + name + `", "version": "1.0.1", "license": "` + license + `"}`,
		"node_modules/" + name + "/LICENSE":      "This is free and unencumbered software released into the public domain.",
	}
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLegacyOnlyOverride(t *testing.T) {
	dir := writeUI(t, "robust-predicates", "Unlicense")
	src, err := newNPMSource(dir, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}

	// The previous UI may use the package under the recorded exception.
	_, problems, err := collectNPM(dir, src, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Errorf("legacy UI: unexpected problems %v", problems)
	}

	// The web UI may not.
	_, problems, err = collectNPM(dir, src, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "legacy UI only") {
		t.Errorf("web UI: problems = %v, want the legacy-only decision", problems)
	}
}
