package maintenance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var ErrExecutorActive = errors.New("un altro ciclo maintenance e gia attivo")

// Lock the existing directory inode, not an unlinkable/recreated lock file.
// Every production execution path enters through RunOnce. The descriptor is
// close-on-exec and its lock is released by the kernel if the process crashes.
func (s *Service) lockExecution() (*os.File, error) {
	path := filepath.Join(s.Root, "maintenance")
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("apertura blocco maintenance: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrExecutorActive
		}
		return nil, fmt.Errorf("blocco maintenance: %w", err)
	}
	return file, nil
}
