//go:build !unix

package repocache

import "io/fs"

func diskBytes(fi fs.FileInfo) int64 { return fi.Size() }

func ownerOf(fs.FileInfo) (int, bool) { return 0, false }
