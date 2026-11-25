// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// RiggerStackOrigin is where a stack sent as files came from: the
// repository its sender fetched it from, as the sender says (a GitOps
// controller knows the commit it applies; the servers can't). The servers
// record it in the operation, and the commit in the releases of the stack's
// own modules. They verify nothing of it: it is what a client says, like
// the files themselves.
type RiggerStackOrigin struct {
	// Repository is the address of the repository, without credentials.
	Repository string `json:"repository,omitempty"`
	// Ref is the branch or the tag.
	Ref string `json:"ref,omitempty"`
	// Commit is the commit.
	Commit string `json:"commit,omitempty"`
	// Signer is who signed the commit or the tag, when the sender verified
	// a signature.
	Signer string `json:"signer,omitempty"`
}

// The bounds of the fields of an origin: it is stored in every operation.
const (
	riggerOriginRepositoryMax = 2048
	riggerOriginFieldMax      = 255
)

// riggerOriginCommit is a commit as git writes one, short or full, of
// SHA-1 or SHA-256.
var riggerOriginCommit = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// checked returns the origin of a request as it is recorded, nil for none.
// An origin is of a stack sent as files (local): a published stack has its
// own source. Its fields are bounded and have no control characters, its
// commit is one, and its repository names no credentials: an address with
// a token in it would be stored in the operation for everyone who reads it.
func (o *RiggerStackOrigin) checked(local bool) (*RiggerStackOrigin, error) {
	if o == nil || *o == (RiggerStackOrigin{}) {
		return nil, nil
	}

	bad := func(format string, args ...any) (*RiggerStackOrigin, error) {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, format, args...)
	}

	if !local {
		return bad("the origin is of a stack sent as files: a published stack has its own source")
	}

	for _, f := range []struct {
		name, value string
		max         int
		spaces      bool
	}{
		{"repository", o.Repository, riggerOriginRepositoryMax, false},
		{"ref", o.Ref, riggerOriginFieldMax, false},
		{"signer", o.Signer, riggerOriginFieldMax, true},
	} {
		if len(f.value) > f.max {
			return bad("the %s of the origin is longer than %d bytes", f.name, f.max)
		}

		if strings.ContainsFunc(f.value, func(r rune) bool { return unicode.IsControl(r) || (!f.spaces && unicode.IsSpace(r)) }) {
			return bad("the %s of the origin has a character that is not allowed in it (a control character or a space)", f.name)
		}
	}

	if o.Commit != "" && !riggerOriginCommit.MatchString(o.Commit) {
		return bad("the commit of the origin is not a commit (7 to 64 hexadecimal digits)")
	}

	if u, err := url.Parse(o.Repository); err == nil && u.User != nil {
		if _, secret := u.User.Password(); secret || u.Scheme == "http" || u.Scheme == "https" {
			return bad("the repository of the origin has credentials in its address: send it without them")
		}
	}

	out := *o

	return &out, nil
}
