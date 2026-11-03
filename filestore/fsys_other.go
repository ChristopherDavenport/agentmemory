//go:build !unix

package filestore

import "io/fs"

// openNonblock is 0 where the platform has no non-blocking open.
const openNonblock = 0

// linkCount reports no link count where the platform gives none
// through the standard library.
func linkCount(fs.FileInfo) (uint64, bool) { return 0, false }
