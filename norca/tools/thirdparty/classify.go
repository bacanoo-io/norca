// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// licenseFileRe matches the names of license files: LICENSE,
	// LICENSE.txt, LICENSE-MIT, LICENCE.md, COPYING, UNLICENSE, ...
	licenseFileRe = regexp.MustCompile(`(?i)^(un)?(licen[cs]e|copying)([._-].*)?$`)
	// noticeFileRe matches the names of NOTICE files.
	noticeFileRe = regexp.MustCompile(`(?i)^notice([._-].*)?$`)
)

// textFile is a license or NOTICE file of a component.
type textFile struct {
	// Path is the path of the file relative to the component root, with
	// forward slashes.
	Path string
	// Text is the normalized content.
	Text string
	// IDs are the SPDX identifiers recognized in a license file.
	IDs []string
}

// findFiles returns the files in dir whose names match re, sorted by name.
func findFiles(dir string, re *regexp.Regexp) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && matchFileName(e.Name(), re) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// matchFileName reports whether a file name matches re and is not a source
// file that happens to match, e.g. license.go.
func matchFileName(name string, re *regexp.Regexp) bool {
	if !re.MatchString(name) {
		return false
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".js", ".mjs", ".cjs", ".ts", ".json", ".sh", ".html", ".css", ".hbs", ".map":
		return false
	}
	return true
}

// collectFiles returns the files matching re that apply to a component rooted
// at root whose code is in the directories dirs (package directories): the
// files in root, and for each directory the files in the nearest directory
// between it and root that has any. Paths are relative to root.
func collectFiles(root string, dirs []string, re *regexp.Regexp) ([]textFile, error) {
	seen := map[string]bool{}
	var rels []string
	add := func(dir string, names []string) {
		for _, n := range names {
			p := filepath.Join(dir, n)
			rel, err := filepath.Rel(root, p)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if !seen[rel] {
				seen[rel] = true
				rels = append(rels, rel)
			}
		}
	}
	add(root, findFiles(root, re))
	for _, d := range dirs {
		for dir := d; ; dir = filepath.Dir(dir) {
			rel, err := filepath.Rel(root, dir)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				break
			}
			if names := findFiles(dir, re); len(names) > 0 {
				add(dir, names)
				break
			}
			if rel == "." {
				break
			}
		}
	}
	sort.Strings(rels)
	out := make([]textFile, 0, len(rels))
	for _, rel := range rels {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		out = append(out, textFile{Path: rel, Text: normalizeText(string(b))})
	}
	return out, nil
}

// normalizeText makes a file's content stable across platforms and safe to
// embed in Markdown: valid UTF-8, LF line endings, no control characters
// other than tab, no trailing whitespace, no leading or trailing blank lines.
func normalizeText(s string) string {
	s = strings.TrimPrefix(s, string(rune(0xFEFF)))
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, string(utf8.RuneError))
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	s = strings.Join(lines, "\n")
	return strings.Trim(s, "\n")
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// canonical reduces a text to lowercase words separated by single spaces, so
// that the license phrases below match regardless of punctuation, line
// breaks and markup.
func canonical(s string) string {
	return " " + strings.TrimSpace(nonAlnum.ReplaceAllString(strings.ToLower(s), " ")) + " "
}

// classify returns the SPDX identifiers of the licenses whose text (or
// standard reference) appears in a license file. A file may contain several
// licenses, e.g. gopkg.in/yaml.v3 (MIT and Apache-2.0).
func classify(text string) []string {
	c := canonical(text)
	has := func(phrases ...string) bool {
		for _, p := range phrases {
			if strings.Contains(c, " "+p+" ") {
				return true
			}
		}
		return false
	}
	var ids []string
	add := func(id string) { ids = append(ids, id) }

	switch {
	case has("mozilla public license version 2 0", "mozilla public license v 2 0", "mozilla public license 2 0", "mpl 2 0"):
		add("MPL-2.0")
	case has("mozilla public license version 1 1", "mozilla public license version 1 0"):
		add("MPL-1.1")
	}
	switch {
	case has("apache license version 2 0", "apache license v2 0", "apache license 2 0", "apache 2 0 license"):
		add("Apache-2.0")
	case has("apache software license version 1 1"):
		add("Apache-1.1")
	}
	if has("permission is hereby granted free of charge to any person obtaining a copy") {
		if has("the above copyright notice and this permission notice shall be included") {
			add("MIT")
		} else {
			add("MIT-0")
		}
	}
	if has("permission to use copy modify and or distribute this software for any purpose with or without fee is hereby granted",
		"permission to use copy modify and distribute this software for any purpose with or without fee is hereby granted") {
		if has("provided that the above copyright notice and this permission notice appear in all copies") {
			add("ISC")
		} else {
			add("0BSD")
		}
	}
	if strings.Contains(c, " redistribution and use in source and binary forms ") && has("are permitted provided that") {
		switch {
		case has("all advertising materials mentioning features or use of this software"):
			add("BSD-4-Clause")
		case has("neither the name", "may not be used to endorse or promote products derived",
			"may be used to endorse or promote products derived"):
			add("BSD-3-Clause")
		default:
			add("BSD-2-Clause")
		}
	}
	if has("cc0 1 0 universal", "creative commons legal code cc0") {
		add("CC0-1.0")
	}
	switch {
	case has("attribution sharealike 4 0 international"):
		add("CC-BY-SA-4.0")
	case has("attribution 4 0 international"):
		add("CC-BY-4.0")
	case has("creative commons attribution"):
		add("CC-BY")
	}
	if has("this is free and unencumbered software released into the public domain") {
		add("Unlicense")
	}
	// The GNU licenses are recognized by their own texts and notices, not by
	// their names: the MPL 2.0 names them as "Secondary Licenses".
	if has("everyone is permitted to copy and distribute verbatim copies of this license document",
		"you can redistribute it and or modify it under the terms of the gnu") {
		add("GPL-family")
	}
	if has("eclipse public license version", "eclipse public license v") {
		add("EPL")
	}
	if has("common development and distribution license cddl version") {
		add("CDDL")
	}
	if has("business source license", "busl 1 1") {
		add("BUSL-1.1")
	}
	if has("server side public license") {
		add("SSPL")
	}
	if has("elastic license 2 0", "elastic license version") {
		add("Elastic")
	}
	if has("the commons clause license condition") {
		add("Commons-Clause")
	}
	return ids
}

// licenseIDs classifies the license files and returns the sorted union of
// the identifiers found.
func licenseIDs(files []textFile) []string {
	set := map[string]bool{}
	for i := range files {
		files[i].IDs = classify(files[i].Text)
		if verbose && len(files[i].IDs) == 0 {
			first, _, _ := strings.Cut(strings.TrimSpace(files[i].Text), "\n")
			fmt.Fprintf(os.Stderr, "no license recognized in %s: %.80s\n", files[i].Path, first)
		}
		for _, id := range files[i].IDs {
			set[id] = true
		}
	}
	return sortedKeys(set)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
