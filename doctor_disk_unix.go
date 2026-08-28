//go:build !windows

package main

import "syscall"

// diskFree reports the bytes available to this user and the total size of the
// filesystem holding path. Split by build tag because Go's standard library has
// no portable way to ask, and periscope ships windows, linux and darwin builds.
//
// Bsize is int64 on linux and uint32 on darwin, so it is widened rather than
// used directly.
func diskFree(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	blockSize := uint64(st.Bsize)
	return st.Bavail * blockSize, st.Blocks * blockSize, nil
}
