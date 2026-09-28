//go:build !windows

package updater

import (
	"os"
	"syscall"
)

// Relaunch replaces the current process image with exe (same PID, args, env).
func Relaunch(exe string) error {
	return syscall.Exec(exe, os.Args, os.Environ())
}
