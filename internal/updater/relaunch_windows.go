//go:build windows

package updater

import (
	"os"
	"os/exec"
)

// Relaunch starts exe as a new process with the same arguments and exits.
// (Windows cannot replace the running process image.)
func Relaunch(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
