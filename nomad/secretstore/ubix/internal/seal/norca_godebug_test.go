// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// The nomad module sets `godebug httpmuxgo121=1` in go.mod for its HTTP API.
// The transit mock of this package's tests routes with the Go 1.22 ServeMux
// patterns ("POST /v1/transit/encrypt/{name}"), which that setting turns off;
// the test binary of this package uses the current ServeMux instead. This
// file is Norca's own: it changes no uBixVault file, so it survives a
// `git subtree pull` unchanged.

//go:debug httpmuxgo121=0

package seal
