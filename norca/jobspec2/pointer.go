// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

// pointerOf returns a pointer to a copy of v. It stands in for the helper
// of the norca program module, which this module does not import.
func pointerOf[T any](v T) *T {
	return &v
}
