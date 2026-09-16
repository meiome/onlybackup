//go:build windows

package cryptfile

// Windows does not expose POSIX directory fsync semantics through os.File.
// File contents are flushed before publication; NTFS metadata durability is
// delegated to the operating system.
func syncDir(string) error { return nil }
