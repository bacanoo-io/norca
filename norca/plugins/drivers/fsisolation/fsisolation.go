// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package fsisolation names the kinds of filesystem isolation a task driver
// provides. The client uses the mode to decide how it builds the task
// directory and which paths it hands to the task.
//
// The package has no dependencies so that drivers and the plugin API can
// both import it; plugins/drivers re-exports the modes as FSIsolation*.
package fsisolation

// Mode is the filesystem isolation of a driver.
type Mode string

const (
	// None runs the task on the host filesystem without restrictions. The
	// task sees the host paths of its directories.
	None = Mode("none")

	// Chroot runs the task in a chroot built from the task directory. The
	// task sees the container paths (/alloc, /local, /secrets).
	Chroot = Mode("chroot")

	// Image runs the task in its own image (for example a container). The
	// task sees the container paths and no host environment.
	Image = Mode("image")

	// Unveil runs the task on the host filesystem, but the driver limits
	// the paths the task may reach (with Landlock on Linux) to its own
	// directories and the paths the operator or the job allow. Like None,
	// the task sees the host paths of its directories.
	Unveil = Mode("unveil")
)

// HostPaths reports whether a task under this mode sees the host paths of
// its directories (rather than container paths).
func (m Mode) HostPaths() bool {
	return m == None || m == Unveil
}
