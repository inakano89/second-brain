//go:build !windows

package updater

import (
	"os"
	"syscall"
)

// Relaunch replaces the current process image with exe (same PID and args),
// optionally adding environment variables ("KEY=value").
func Relaunch(exe string, env ...string) error {
	return syscall.Exec(exe, append([]string{exe}, os.Args[1:]...), append(os.Environ(), env...))
}
