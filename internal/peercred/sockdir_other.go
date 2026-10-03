//go:build !unix

package peercred

import "io/fs"

// PrepareDir: without Unix sockets there is nothing to prepare.
func PrepareDir(string) (bool, error) { return false, ErrUnsupported }

// ClearPath: see PrepareDir.
func ClearPath(string) error { return ErrUnsupported }

// OwnerOf: see PrepareDir.
func OwnerOf(fs.FileInfo) (int, int, bool) { return -1, -1, false }
