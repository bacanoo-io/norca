// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// A series file that cannot be written is reported, and the series stay
// marked as changed, so the next save writes them.
func TestStore_SaveSeriesFailure(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	s := openTest(t, testConfig(dir))

	base := time.Now().Add(-time.Hour).Truncate(30 * time.Second)
	must.True(t, s.IngestStream(usage("api", 1, 10, 20), base.Add(time.Second), 1))

	// A directory in place of the temporary file makes the write fail.
	tmp := filepath.Join(dir, seriesFile+".tmp")
	must.NoError(t, os.Mkdir(tmp, 0o700))

	must.Error(t, s.saveSeries())
	s.mu.Lock()
	dirty := s.seriesDirty
	s.mu.Unlock()
	must.True(t, dirty)

	must.NoError(t, os.Remove(tmp))
	must.NoError(t, s.saveSeries())
	_, err := os.Stat(filepath.Join(dir, seriesFile))
	must.NoError(t, err)
	must.NoError(t, s.Close())
}
