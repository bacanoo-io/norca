// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/opencontainers/runc/libcontainer"
	lutils "github.com/opencontainers/runc/libcontainer/utils"
	"github.com/shoenig/test/must"
	"golang.org/x/sys/unix"
)

func TestSetProcessUser(t *testing.T) {
	ci.Parallel(t)

	rootfs := t.TempDir()
	must.NoError(t, os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755))
	must.NoError(t, os.WriteFile(filepath.Join(rootfs, "etc", "passwd"), []byte(
		"root:x:0:0:root:/root:/bin/sh\n"+
			"nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n"+
			"alice:x:1001:1001:alice:/home/alice:/bin/sh\n"), 0o644))
	must.NoError(t, os.WriteFile(filepath.Join(rootfs, "etc", "group"), []byte(
		"root:x:0:\n"+
			"nogroup:x:65534:\n"+
			"alice:x:1001:\n"+
			"video:x:44:alice\n"+
			"audio:x:29:alice,bob\n"), 0o644))

	cases := []struct {
		name  string
		spec  string
		uid   int
		gid   int
		sgids []int
	}{
		{name: "default root", spec: "", uid: 0, gid: 0},
		{name: "by name", spec: "nobody", uid: 65534, gid: 65534},
		{name: "supplementary groups", spec: "alice", uid: 1001, gid: 1001, sgids: []int{44, 29}},
		{name: "numeric without entry", spec: "2000:3000", uid: 2000, gid: 3000},
		{name: "name and group", spec: "alice:video", uid: 1001, gid: 44},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &libcontainer.Process{}
			must.NoError(t, setProcessUser(p, rootfs, tc.spec))
			must.Eq(t, tc.uid, p.UID)
			must.Eq(t, tc.gid, p.GID)
			must.SliceContainsAll(t, tc.sgids, p.AdditionalGroups)
		})
	}

	t.Run("unknown user", func(t *testing.T) {
		p := &libcontainer.Process{}
		must.Error(t, setProcessUser(p, rootfs, "mallory"))
	})
}

func TestSetProcessUser_SymlinkStaysInRootfs(t *testing.T) {
	ci.Parallel(t)

	// a passwd file outside of the rootfs that must never be read
	outside := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(outside, "passwd"), []byte(
		"evil:x:4242:4242::/:/bin/sh\n"), 0o644))

	rootfs := t.TempDir()
	must.NoError(t, os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755))
	must.NoError(t, os.Symlink(filepath.Join(outside, "passwd"), filepath.Join(rootfs, "etc", "passwd")))

	p := &libcontainer.Process{}
	must.Error(t, setProcessUser(p, rootfs, "evil"))
}

func TestRecvFile(t *testing.T) {
	ci.Parallel(t)

	parent, child, err := lutils.NewSockPair("test")
	must.NoError(t, err)
	defer parent.Close()
	defer child.Close()

	f, err := os.CreateTemp(t.TempDir(), "recv")
	must.NoError(t, err)
	defer f.Close()
	_, err = f.WriteString("hello")
	must.NoError(t, err)

	// send the file the way the container init sends the console
	err = unix.Sendmsg(int(child.Fd()), []byte(f.Name()), unix.UnixRights(int(f.Fd())), nil, 0)
	must.NoError(t, err)

	got, err := recvFile(parent)
	must.NoError(t, err)
	defer got.Close()
	must.Eq(t, f.Name(), got.Name())

	buf := make([]byte, 5)
	_, err = got.ReadAt(buf, 0)
	must.NoError(t, err)
	must.Eq(t, "hello", string(buf))
}
