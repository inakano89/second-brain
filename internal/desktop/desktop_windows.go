//go:build windows

package desktop

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey         = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue       = "SecondBrain"
	createNoWindow = 0x08000000 // CREATE_NO_WINDOW: console app without a window; children inherit it
)

// Supported reports whether login start and background mode are available here.
func Supported() bool { return true }

// Autostart returns the registered login command, if any.
func Autostart() (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValue)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetAutostart registers (command != "") or removes the login entry for the current user.
func SetAutostart(command string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if command == "" {
		if err := k.DeleteValue(runValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	return k.SetStringValue(runValue, command)
}

// Detach starts a windowless copy of this process (same arguments) writing to
// logPath, then exits.
func Detach(logPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	trimLog(logPath, 20<<20)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.Env = append(os.Environ(), DetachedEnv+"=1")
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}
	cmd.SysProcAttr = NoWindow()
	if err := cmd.Start(); err != nil {
		f.Close()
		return err
	}
	os.Exit(0)
	return nil
}

// NoWindow keeps child processes of a background server from opening console windows.
func NoWindow() *syscall.SysProcAttr {
	if !Detached() {
		return nil
	}
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// OpenBrowser opens url in the default browser.
func OpenBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
