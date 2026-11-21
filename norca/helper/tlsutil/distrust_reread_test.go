// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tlsutil

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// A list replaced so that its modification time and size stay is read again:
// at once when another file is renamed over it, and within distrustReread
// when it is written in place (review finding HLP-10).
func TestDistrustedCA_ReplacedUnnoticed(t *testing.T) {
	ci.Parallel(t)
	p := newTestPKI(t)
	first := p.nodeCA("node CA 1", p.key())
	second := p.nodeCA("node CA 2", p.key())

	list := p.write("distrusted", nil, first)
	info, err := os.Stat(list)
	must.NoError(t, err)
	d, err := newDistrustList(list)
	must.NoError(t, err)
	clock := time.Now()
	d.now = func() time.Time { return clock }
	d.readAt = clock
	has := func(c []byte) bool {
		_, ok := d.current()[sha256.Sum256(c)]
		return ok
	}
	must.True(t, has(first.RawSubjectPublicKeyInfo))

	// Written in place; the modification time and the size are made the
	// ones of the file read, as a write within the resolution of the time or
	// a copy that keeps it does.
	p.write("distrusted", nil, second)
	must.NoError(t, os.Chtimes(list, info.ModTime(), info.ModTime()))
	now, err := os.Stat(list)
	must.NoError(t, err)
	d.size, d.ctime = now.Size(), changeTime(now)
	must.True(t, has(first.RawSubjectPublicKeyInfo), must.Sprint("nothing tells the change yet"))
	clock = clock.Add(distrustReread)
	must.True(t, has(second.RawSubjectPublicKeyInfo))
	must.False(t, has(first.RawSubjectPublicKeyInfo))

	// Another file renamed over it, with the same times and size.
	other := filepath.Join(filepath.Dir(list), "other.pem")
	data, err := os.ReadFile(p.write("other", nil, first))
	must.NoError(t, err)
	must.NoError(t, os.WriteFile(other, data, 0o600))
	must.NoError(t, os.Chtimes(other, info.ModTime(), info.ModTime()))
	must.NoError(t, os.Rename(other, list))
	renamed, err := os.Stat(list)
	must.NoError(t, err)
	d.size, d.ctime = renamed.Size(), changeTime(renamed)
	must.True(t, has(first.RawSubjectPublicKeyInfo))
}
