//go:build unix

package filestore

import (
	"io/fs"
	"syscall"
)

// openNonblock keeps an open from blocking on a FIFO swapped in for a
// file between the check and the open; a regular file ignores it.
const openNonblock = syscall.O_NONBLOCK

// linkCount returns a file's number of hard links, and whether the
// platform reports one.
func linkCount(info fs.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}
