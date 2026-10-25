// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package executor

import (
	"fmt"
	"os"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/moby/sys/user"
	"github.com/opencontainers/runc/libcontainer"
	"golang.org/x/sys/unix"
)

// setProcessUser resolves userSpec ("", "user", "uid", "user:group",
// "uid:gid", ...) against the /etc/passwd and /etc/group files of the
// container root filesystem and sets the numeric credentials on process.
//
// Older libcontainer releases accepted the user as a string and resolved it
// inside the container during init; libcontainer now takes numeric IDs, so the
// lookup happens here with the same defaults (uid 0, gid 0) and the same
// supplementary group semantics (the groups listing the user as a member).
func setProcessUser(process *libcontainer.Process, rootfs, userSpec string) error {
	// resolve the files inside rootfs so symlinks cannot point outside of it
	passwdPath, err := securejoin.SecureJoin(rootfs, "/etc/passwd")
	if err != nil {
		return fmt.Errorf("failed to resolve passwd file in %s: %w", rootfs, err)
	}
	groupPath, err := securejoin.SecureJoin(rootfs, "/etc/group")
	if err != nil {
		return fmt.Errorf("failed to resolve group file in %s: %w", rootfs, err)
	}

	defaults := &user.ExecUser{Uid: 0, Gid: 0, Home: "/"}
	execUser, err := user.GetExecUserPath(userSpec, defaults, passwdPath, groupPath)
	if err != nil {
		return fmt.Errorf("failed to look up user %q: %w", userSpec, err)
	}

	process.UID = execUser.Uid
	process.GID = execUser.Gid
	process.AdditionalGroups = execUser.Sgids
	return nil
}

// recvFile receives a single file descriptor (and its name) sent over the
// AF_UNIX socket by the container init process, e.g. the console pty master.
func recvFile(socket *os.File) (*os.File, error) {
	const maxNameLen = 4096
	name := make([]byte, maxNameLen)
	oobSpace := unix.CmsgSpace(4)
	oob := make([]byte, oobSpace)

	var (
		n, oobn int
		err     error
	)
	for {
		n, oobn, _, _, err = unix.Recvmsg(int(socket.Fd()), name, oob, unix.MSG_CMSG_CLOEXEC)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		return nil, os.NewSyscallError("recvmsg", err)
	}
	if n >= maxNameLen || oobn != oobSpace {
		return nil, fmt.Errorf("recvfile: unexpected message size (n=%d oobn=%d)", n, oobn)
	}

	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}

	var fds []int
	for i := range msgs {
		if msgs[i].Header.Type != unix.SCM_RIGHTS {
			continue
		}
		rights, err := unix.ParseUnixRights(&msgs[i])
		if err != nil {
			closeFds(fds)
			return nil, err
		}
		fds = append(fds, rights...)
	}
	if len(fds) != 1 {
		closeFds(fds)
		return nil, fmt.Errorf("recvfile: expected 1 file descriptor, got %d", len(fds))
	}
	return os.NewFile(uintptr(fds[0]), string(name[:n])), nil
}

func closeFds(fds []int) {
	for _, fd := range fds {
		_ = unix.Close(fd)
	}
}
