// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"errors"
	"net"
)

// What the cache has serves in the place of a fetch only when the other
// side can't be reached (a machine without a network: design 7.3, 12.6).
// Every other failure is the answer: a server that refuses the caller,
// credentials that can't be read, a certificate that is not trusted, an
// index that does not hold, a request that ended. A fallback on those would
// give a caller what it was just refused, or what it gets by ending its own
// request.

// errNoSSHAgent is the error of an ssh agent that this machine does not
// run. It is the program's own way to a repository, the same for every
// caller, and without it the repository is not reached.
var errNoSSHAgent = errors.New("no ssh agent")

// unreachable reports whether a fetch failed because the other side could
// not be reached at all: no connection was made (no network, no such host,
// nothing listens), or the ssh agent to connect with is not there.
func unreachable(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	if errors.Is(err, errNoSSHAgent) {
		return true
	}

	// A dial that failed, also the one inside the error of a proxy.
	for op := (*net.OpError)(nil); errors.As(err, &op); err = op.Err {
		if op.Op == "dial" {
			return true
		}
	}

	return false
}
