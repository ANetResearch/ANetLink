//go:build unix && !linux

package peercred

// maxSocketPath is sizeof(sun_path) minus the terminating NUL on the BSDs
// and macOS.
const maxSocketPath = 103
