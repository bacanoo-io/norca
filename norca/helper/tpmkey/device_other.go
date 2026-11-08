// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package tpmkey

import "github.com/google/go-tpm/tpm2/transport"

// Supported reports whether TPM keys are supported on this system.
const Supported = false

// DeviceOpener returns ErrUnsupported: TPM keys are supported on Linux only.
func DeviceOpener(string) Opener {
	return func() (transport.TPMCloser, error) { return nil, ErrUnsupported }
}

// Present is false: TPM keys are supported on Linux only.
func Present(string) bool { return false }
