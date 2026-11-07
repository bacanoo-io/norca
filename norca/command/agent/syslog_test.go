// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"runtime"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	gsyslog "github.com/hashicorp/go-syslog"
	"github.com/hashicorp/logutils"
)

func TestSyslogFilter(t *testing.T) {
	ci.Parallel(t)
	if runtime.GOOS == "windows" {
		t.Skip("Syslog not supported on Windows")
	}
	if os.Getenv("TRAVIS") == "true" {
		t.Skip("Syslog not supported on travis-ci")
	}
	// The local syslog socket go-syslog dials; a container has none (Norca).
	if !hasSyslogSocket() {
		t.Skip("no local syslog socket (/dev/log, /var/run/syslog, /var/run/log)")
	}

	l, err := gsyslog.NewLogger(gsyslog.LOG_NOTICE, "LOCAL0", "consul")
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	filt := LevelFilter()
	filt.MinLevel = logutils.LogLevel("INFO")

	s := &SyslogWrapper{l, filt}
	n, err := s.Write([]byte("[INFO] test"))
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if n == 0 {
		t.Fatalf("should have logged")
	}

	n, err = s.Write([]byte("[DEBUG] test"))
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if n != 0 {
		t.Fatalf("should not have logged")
	}
}

// hasSyslogSocket reports whether one of the sockets go-syslog dials exists.
func hasSyslogSocket() bool {
	for _, p := range []string{"/dev/log", "/var/run/syslog", "/var/run/log"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}
