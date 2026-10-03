//go:build !linux && !darwin && !freebsd

package peercred

const supported = false

func fromFD(int) (Cred, error) { return unknown, ErrUnsupported }
