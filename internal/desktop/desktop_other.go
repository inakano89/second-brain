//go:build !windows

package desktop

import (
	"errors"
	"syscall"
)

var errUnsupported = errors.New("disponível apenas no Windows (no Linux use o serviço systemd ou Docker)")

// Supported reports whether login start and background mode are available here.
func Supported() bool { return false }

// Autostart returns the registered login command, if any.
func Autostart() (string, bool, error) { return "", false, nil }

// SetAutostart registers (command != "") or removes the login entry.
func SetAutostart(string) error { return errUnsupported }

// Detach starts a windowless copy of this process and exits.
func Detach(string) error { return errUnsupported }

// NoWindow keeps child processes from opening console windows (Windows only).
func NoWindow() *syscall.SysProcAttr { return nil }

// OpenBrowser opens url in the default browser.
func OpenBrowser(string) error { return errUnsupported }
