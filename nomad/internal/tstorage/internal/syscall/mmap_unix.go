// Modified by Norca contributors: munmap added, build constraint syntax.

//go:build !windows && !plan9

package syscall

import "syscall"

func mmap(fd, length int) ([]byte, error) {
	return syscall.Mmap(
		fd,
		0,
		length,
		syscall.PROT_READ,
		syscall.MAP_SHARED,
	)
}

func munmap(b []byte) error {
	return syscall.Munmap(b)
}
