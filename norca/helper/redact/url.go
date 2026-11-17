// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package redact keeps the credentials of configured URLs out of the errors
// of the HTTP calls made to them. Such errors reach the logs, and callers
// with fewer rights than whoever configured the URL: the submitter of a job
// an admission webhook reviews, the reader of the status of a scaling
// policy, somebody who logs in.
//
// The error of net/http names the URL of the request. Go hides the password
// in it and nothing else: a token written as the user name
// ("https://token@host") and the values of the query ("?api_key=...") stay.
package redact

import (
	"errors"
	"net/url"
	"sort"
	"strings"
)

const (
	// userPlaceholder replaces the user information of a URL.
	userPlaceholder = "xxxxx"

	// valuePlaceholder replaces any other credential.
	valuePlaceholder = "<redacted>"
)

// URLError returns the reason of a failed HTTP call without the URL of the
// request: the error inside the *url.Error of net/http (a dial, TLS or
// timeout error, which names at most a host and a port). errors.Is and
// errors.As keep working on the reason. Other errors are returned as they
// are.
func URLError(err error) error {
	for {
		var urlErr *url.Error
		if !errors.As(err, &urlErr) || urlErr.Err == nil {
			return err
		}
		err = urlErr.Err
	}
}

// URLSecrets returns text without the credentials of the given URLs, in
// whatever form a library wrote them into it: the user information (also
// as Go prints it, with the password hidden) is replaced by "xxxxx", the
// values of the query parameters and the fragment by "<redacted>". A URL
// that can't be parsed is replaced as a whole. The scheme, host and path
// stay, they say which call failed.
func URLSecrets(text string, rawURLs ...string) string {
	if text == "" {
		return text
	}
	type replacement struct{ old, new string }
	var reps []replacement
	add := func(old, new string) {
		if old != "" && old != new {
			reps = append(reps, replacement{old, new})
		}
	}

	for _, raw := range rawURLs {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			add(raw, valuePlaceholder)
			continue
		}

		if u.User != nil {
			name := u.User.Username()
			password, hasPassword := u.User.Password()
			forms := []string{u.User.String()}
			if name != "" {
				forms = append(forms, name, url.User(name).String())
			}
			if hasPassword {
				forms = append(forms, name+":"+password)
				// With the password hidden, whatever stands for it:
				// (*url.URL).Redacted writes "xxxxx", the errors of
				// net/http "***".
				text = hiddenPasswordForms(text, name)
				text = hiddenPasswordForms(text, url.User(name).String())
			}
			// As written in the URL, whatever its escaping is.
			if rest, ok := strings.CutPrefix(raw, u.Scheme+"://"); ok {
				if userinfo, _, ok := strings.Cut(rest, "@"); ok && !strings.ContainsAny(userinfo, "/?#") {
					forms = append(forms, userinfo)
				}
			}
			for _, form := range forms {
				add(form+"@", userPlaceholder+"@")
			}
		}

		for _, param := range strings.Split(u.RawQuery, "&") {
			name, value, found := strings.Cut(param, "=")
			switch {
			case found && value != "":
				add(param, name+"="+valuePlaceholder)
			case !found && param != "":
				// A parameter without "=" may be a bare token.
				add("?"+param, "?"+valuePlaceholder)
				add("&"+param, "&"+valuePlaceholder)
			}
		}
		for name, values := range u.Query() {
			for _, value := range values {
				if value == "" {
					continue
				}
				add(name+"="+value, name+"="+valuePlaceholder)
				add(url.QueryEscape(name)+"="+url.QueryEscape(value), url.QueryEscape(name)+"="+valuePlaceholder)
			}
		}

		if u.Fragment != "" {
			add("#"+u.Fragment, "#"+valuePlaceholder)
			add("#"+u.EscapedFragment(), "#"+valuePlaceholder)
		}
	}

	// The longest first, so that no form is cut by a shorter one inside it.
	sort.SliceStable(reps, func(i, j int) bool { return len(reps[i].old) > len(reps[j].old) })
	for _, r := range reps {
		text = strings.ReplaceAll(text, r.old, r.new)
	}
	return text
}

// hiddenPasswordForms replaces every "name:<mask>@" of text, the user
// information of a URL printed with its password hidden by some mask, with
// "xxxxx@".
func hiddenPasswordForms(text, name string) string {
	prefix := name + ":"
	var out strings.Builder
	for {
		i := strings.Index(text, prefix)
		if i < 0 {
			break
		}
		rest := text[i+len(prefix):]
		at := strings.IndexByte(rest, '@')
		// The mask is short and has nothing that ends a user information.
		if at < 0 || at > 64 || strings.ContainsAny(rest[:at], "/?# \t\r\n\"'") {
			out.WriteString(text[:i+len(prefix)])
			text = rest
			continue
		}
		out.WriteString(text[:i])
		out.WriteString(userPlaceholder)
		text = rest[at:]
	}
	out.WriteString(text)
	return out.String()
}

// CallError returns the error of a failed HTTP call to one of the given
// configured URLs in a form that may be shown and logged: without the URL of
// the request when the error carries it (URLError), and without the
// credentials of the configured URLs anywhere in its text (URLSecrets). When
// the text had to be changed the result is a new error with that text.
func CallError(err error, rawURLs ...string) error {
	if err == nil {
		return nil
	}
	err = URLError(err)
	text := err.Error()
	if clean := URLSecrets(text, rawURLs...); clean != text {
		return errors.New(clean)
	}
	return err
}
