// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package snapshot

import (
	"errors"
	"io"
)

// NewReader returns an independent reader of the whole snapshot archive, so
// the archive can be read several times (for example once per backup
// destination) without seeking the shared file.
func (s *Snapshot) NewReader() (io.Reader, error) {
	if s == nil {
		return nil, errors.New("no snapshot")
	}
	info, err := s.file.Stat()
	if err != nil {
		return nil, err
	}
	return io.NewSectionReader(s.file, 0, info.Size()), nil
}
