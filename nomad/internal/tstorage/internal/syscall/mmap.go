// Modified by Norca contributors: Munmap added.

package syscall

// Mmap maps length bytes of the file fd read-only into memory.
func Mmap(fd, length int) ([]byte, error) {
	return mmap(fd, length)
}

// Munmap unmaps memory mapped by Mmap.
func Munmap(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return munmap(b)
}
