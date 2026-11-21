// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package tlsutil

import (
	"os"
	"time"
)

// changeTime is not known here: the file, its modification time and size,
// and the periodic read tell a change.
func changeTime(os.FileInfo) time.Time { return time.Time{} }
