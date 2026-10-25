// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package useragent

import (
	"fmt"
	"net/http"
	"runtime"

	"github.com/hashicorp/nomad/version"
)

const (
	// Header is the User-Agent header key
	// https://www.rfc-editor.org/rfc/rfc7231#section-5.5.3
	Header = `User-Agent`
)

var (
	// norcaVersionFunc returns the Norca product version. A variable for tests.
	norcaVersionFunc = func() string {
		return version.NorcaVersion
	}

	// rt is the runtime - variable for tests.
	rt = runtime.Version()

	// versionFunc is the func that returns the current version. This is a
	// function to take into account the different build processes and distinguish
	// between enterprise and oss builds.
	versionFunc = func() string {
		return version.GetVersion().VersionNumber()
	}
)

// String returns the consistent user-agent string for Norca, naming the
// Norca product version and the Nomad API version it is compatible with, e.g.
// "Norca/0.1.0 (Nomad API 1.6.5; go1.27.3)".
func String() string {
	return fmt.Sprintf("Norca/%s (Nomad API %s; %s)",
		norcaVersionFunc(), versionFunc(), rt)
}

// HeaderSetter is anything that implements SetHeaders(http.Header).
type HeaderSetter interface {
	SetHeaders(http.Header)
}

// SetHeaders configures the User-Agent http.Header for the client.
func SetHeaders(client HeaderSetter) {
	client.SetHeaders(http.Header{
		Header: []string{String()},
	})
}
