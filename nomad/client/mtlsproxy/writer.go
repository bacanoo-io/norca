// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtlsproxy

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"github.com/hashicorp/nomad/helper/users"
)

// Writer writes configuration files into a directory. A file is only
// replaced when its content changes, and always atomically (written next to
// it and renamed into place), which is what makes Envoy reload it. EDS files
// that are no longer part of the configuration are removed after the
// clusters referring to them are gone.
type Writer struct {
	// Dir is the configuration directory on the client.
	Dir string

	// User owns the files, like the other files of the secrets directory.
	User string

	written map[string][]byte
}

// writeOrder returns the rank of a file in a write: the secrets and
// endpoints a cluster or listener refers to are written first, then the
// clusters, then the listeners referring to them.
func writeOrder(name string) int {
	switch name {
	case CertSecretFile, TrustSecretFile:
		return 0
	case ClustersFile:
		return 2
	case ListenersFile:
		return 3
	case BootstrapFile:
		return 4
	default:
		return 1
	}
}

// Write brings the directory to files and returns whether it changed
// anything.
func (w *Writer) Write(files map[string][]byte) (bool, error) {
	if w.written == nil {
		w.written = make(map[string][]byte)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		oi, oj := writeOrder(names[i]), writeOrder(names[j])
		if oi != oj {
			return oi < oj
		}
		return names[i] < names[j]
	})

	changed := false
	for _, name := range names {
		content := files[name]
		prev, ok := w.written[name]
		if !ok {
			// after a client restart, compare with the file on disk
			prev, _ = os.ReadFile(filepath.Join(w.Dir, name))
		}
		if prev != nil && bytes.Equal(prev, content) {
			w.written[name] = prev
			continue
		}
		if err := w.writeFile(name, content); err != nil {
			return changed, err
		}
		w.written[name] = content
		changed = true
	}

	// remove the endpoints of clusters that are gone
	entries, err := os.ReadDir(w.Dir)
	if err != nil {
		return changed, err
	}
	for _, e := range entries {
		name := e.Name()
		if _, ok := files[name]; ok || !IsEndpointsFile(name) {
			continue
		}
		if err := os.Remove(filepath.Join(w.Dir, name)); err != nil && !os.IsNotExist(err) {
			return changed, err
		}
		delete(w.written, name)
		changed = true
	}
	return changed, nil
}

func (w *Writer) writeFile(name string, content []byte) error {
	path := filepath.Join(w.Dir, name)
	tmp := filepath.Join(w.Dir, fmt.Sprintf(".%s.tmp%d", name, rand.Int63n(math.MaxInt32)))
	if err := users.WriteFileFor(tmp, content, w.User); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to write %s: %w", name, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to write %s: %w", name, err)
	}
	return nil
}
