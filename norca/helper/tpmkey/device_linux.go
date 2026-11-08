// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tpmkey

import (
	"os"

	"github.com/google/go-tpm/tpm2/transport"
	"github.com/google/go-tpm/tpm2/transport/linuxtpm"
)

// Supported reports whether TPM keys are supported on this system.
const Supported = true

// DeviceOpener opens the TPM device at path (DefaultDevice when empty).
func DeviceOpener(path string) Opener {
	if path == "" {
		path = DefaultDevice
	}
	return func() (transport.TPMCloser, error) { return linuxtpm.Open(path) }
}

// Present reports whether path (DefaultDevice when empty) is a character
// device: a TPM, if it is the TPM device.
func Present(path string) bool {
	if path == "" {
		path = DefaultDevice
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
