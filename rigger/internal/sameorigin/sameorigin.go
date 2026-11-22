// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package sameorigin keeps credentials to the origin they are for: a
// scheme, host and port. net/http copies the Authorization header to a
// redirect on the same host name whatever its scheme or port.
package sameorigin

import (
	"cmp"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// maxRedirects is the limit of net/http, which a CheckRedirect replaces.
const maxRedirects = 10

var errRedirects = errors.New("too many redirects")

// Of is the origin of a URL, its default port written out.
func Of(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)

	port := "80"
	if scheme == "https" {
		port = "443"
	}

	return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + cmp.Or(u.Port(), port)
}

// CheckRedirect is for http.Client: it follows a redirect without the
// Authorization header when it leads to another origin than the first
// request.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errRedirects
	}

	if len(via) > 0 && Of(req.URL) != Of(via[0].URL) {
		req.Header.Del("Authorization")
	}

	return nil
}
