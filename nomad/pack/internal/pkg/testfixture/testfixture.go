// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package testfixture

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/shoenig/test/must"
)

var RelFixtureDir = "fixtures"

// AbsPath returns the absolute path to a fixture inside the FixtureDir
func AbsPath(t *testing.T, fixtureName string) string {
	t.Helper()
	dir, err := fixtureDir()
	must.NoError(t, err)
	return filepath.Join(dir, filepath.FromSlash(fixtureName))
}

// Clone creates a test TempDir, copies the given Pack into it, and returns the
// absolute path to the copy.
func Clone(t *testing.T, fPath string) (dest string) {
	t.Helper()
	dest, err := copyFixture(t.TempDir(), fPath)
	must.NoError(t, err)
	return dest
}

// MustAbsPath returns the absolute path to a fixture inside the FixtureDir
func MustAbsPath(fixtureName string) string {
	dir, err := fixtureDir()
	if err != nil {
		panic(err)
	}
	return filepath.Join(dir, filepath.FromSlash(fixtureName))
}

// Clone creates a test TempDir, copies the given Pack into it, and returns the
// absolute path to the copy.
func MustClone(dst string, fPath string) (dest string) {
	dest, err := copyFixture(dst, fPath)
	if err != nil {
		panic(fmt.Sprintf("MustClone fatal error: %v", err))
	}
	return dest
}

// copyFixture copies the fixture directory fPath into dst and returns the
// path of the copy.
func copyFixture(dst, fPath string) (string, error) {
	src, err := fixtureDir()
	if err != nil {
		return "", err
	}
	src = filepath.Join(src, filepath.FromSlash(fPath))
	dest := filepath.Join(dst, path.Base(fPath))
	if err := os.CopyFS(dest, os.DirFS(src)); err != nil {
		return "", fmt.Errorf("copying fixture %q: %w", fPath, err)
	}
	return dest, nil
}

// fixtureDir returns the absolute path of the fixtures folder of the pack
// source tree. It is derived from the location of this source file, so it does
// not depend on the git repository layout, the test's working directory or
// os.Chdir calls in parallel tests.
func fixtureDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("unable to determine the fixture directory")
	}
	// file is <pack>/internal/pkg/testfixture/testfixture.go
	return filepath.Abs(filepath.Join(filepath.Dir(file), "..", "..", "..", RelFixtureDir))
}
