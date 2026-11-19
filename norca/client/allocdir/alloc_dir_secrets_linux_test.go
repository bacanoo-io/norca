// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package allocdir

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// A task that swaps a link between an ordinary file and its secret while
// ReadAt runs gets no secret either: a check of the path before the open
// alone loses that race, so ReadAt checks the file it opened by the name of
// its descriptor (review finding JOG-15).
func TestAllocDir_ReadAt_SwappedLink(t *testing.T) {
	ci.Parallel(t)
	d, td := newSecretsTestDir(t)

	link := filepath.Join(td.LocalDir, "swap")
	tmp := filepath.Join(td.LocalDir, "swap.tmp")
	must.NoError(t, os.Symlink("plain", link))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		targets := []string{"plain", filepath.Join("..", TaskSecrets, "norca_token")}
		for i := 0; ctx.Err() == nil; i++ {
			_ = os.Remove(tmp)
			if os.Symlink(targets[i%2], tmp) == nil {
				_ = os.Rename(tmp, link)
			}
		}
	}()
	defer func() {
		cancel()
		<-done
	}()

	path := filepath.Join(t1.Name, TaskLocal, "swap")
	reads := 0
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		r, err := d.ReadAt(path, 0)
		if err != nil {
			continue
		}
		must.Eq(t, "plain", readAll(t, r))
		reads++
	}
	must.Positive(t, reads)
}
