//go:build linux

package collect

import "syscall"

// Disk is the filesystem holding path: bytes used and its size. Used counts
// the blocks reserved for root, as df does not, because a disk whose
// reserve is all that is left is full for everything but root.
func Disk(path string) (used, total uint64, err error) {
	var stat syscall.Statfs_t

	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}

	size := uint64(stat.Bsize)
	total = stat.Blocks * size
	used = (stat.Blocks - stat.Bfree) * size

	return used, total, nil
}
