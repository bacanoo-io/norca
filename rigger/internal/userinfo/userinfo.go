// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package userinfo finds the user and the password in an address by its
// text, as net/url splits an authority: an address that does not parse is
// printed too, and the error of a parser repeats it.
package userinfo

import (
	"regexp"
	"strings"
)

// Mask stands for a user and a password in an address that is printed.
const Mask = "xxxxx"

var (
	schemePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
	drivePattern  = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
)

// Find returns the user and password of the authority of an address (what
// is before its last at sign), where they start, and the scheme in lower
// case ("" without one: a host and a path). ok is false when the address
// has none; a path of the file system has none.
func Find(address string) (scheme, info string, start int, ok bool) {
	rest := address

	if m := schemePattern.FindString(address); m != "" {
		scheme, start, rest = strings.ToLower(strings.TrimSuffix(m, "://")), len(m), address[len(m):]
	}

	if scheme == "file" || scheme == "dir" || strings.HasPrefix(address, "/") || drivePattern.MatchString(address) {
		return scheme, "", 0, false
	}

	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}

	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return scheme, "", 0, false
	}

	return scheme, authority[:at], start, true
}

// Redact returns an address with Mask in the place of its user and
// password.
func Redact(address string) string {
	_, info, start, ok := Find(address)
	if !ok {
		return address
	}

	return address[:start] + Mask + address[start+len(info):]
}
