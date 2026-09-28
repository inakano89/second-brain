package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// marker tracks an update that has not yet been confirmed healthy.
type marker struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	At       time.Time `json:"at"`
	Attempts int       `json:"attempts"`
}

// maxBootAttempts is how many unconfirmed starts a new version gets before rollback.
const maxBootAttempts = 3

func markerPath(exe string) string { return exe + ".update.json" }
func skipPath(exe string) string   { return exe + ".skip" }

func readMarker(exe string) (*marker, error) {
	b, err := os.ReadFile(markerPath(exe))
	if err != nil {
		return nil, err
	}
	var m marker
	return &m, json.Unmarshal(b, &m)
}

func writeMarker(exe string, m marker) error {
	b, _ := json.Marshal(m)
	return os.WriteFile(markerPath(exe), b, 0o600)
}

func isSkipped(exe, tag string) bool {
	b, err := os.ReadFile(skipPath(exe))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == tag {
			return true
		}
	}
	return false
}

func addSkip(exe, tag string) {
	f, err := os.OpenFile(skipPath(exe), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintln(f, tag)
	f.Close()
}

// Executable returns the resolved path of the running binary.
func Executable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		return r
	}
	return exe
}

// Recover must run first thing at startup. It counts boots of a freshly
// installed version; after maxBootAttempts unconfirmed starts it restores
// exe.old, blacklists the bad version and relaunches the previous binary.
func Recover(exe string, logf func(format string, a ...any)) {
	if !recoverStep(exe, logf) {
		return
	}
	if err := Relaunch(exe); err != nil {
		logf("rollback concluído; reinicie manualmente: %v", err)
		os.Exit(1)
	}
}

// recoverStep updates the boot counter and performs the file rollback when due.
func recoverStep(exe string, logf func(format string, a ...any)) (rolledBack bool) {
	if exe == "" {
		return false
	}
	m, err := readMarker(exe)
	if err != nil {
		return false
	}
	m.Attempts++
	if m.Attempts <= maxBootAttempts {
		_ = writeMarker(exe, *m)
		return false
	}
	old := exe + ".old"
	if _, err := os.Stat(old); err != nil {
		logf("atualização para %s não confirmada, mas %s não existe; mantendo versão atual", m.To, old)
		os.Remove(markerPath(exe))
		return false
	}
	logf("versão %s falhou %d inicializações; revertendo para %s", m.To, m.Attempts-1, m.From)
	failed := exe + ".failed"
	os.Remove(failed)
	if err := os.Rename(exe, failed); err != nil {
		logf("rollback: %v", err)
		return false
	}
	if err := os.Rename(old, exe); err != nil {
		_ = os.Rename(failed, exe)
		logf("rollback: %v", err)
		return false
	}
	addSkip(exe, m.To)
	os.Remove(markerPath(exe))
	return true
}

// ConfirmAfter marks the running version healthy after d without shutdown,
// removing the rollback marker and notifying the user.
func (u *Updater) ConfirmAfter(ctx context.Context, d time.Duration) {
	m, err := readMarker(u.exe)
	if err != nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(d):
	}
	if err := os.Remove(markerPath(u.exe)); err != nil && !errors.Is(err, os.ErrNotExist) {
		u.log.Warn("falha ao confirmar atualização", "err", err)
		return
	}
	u.log.Info("atualização confirmada", "from", m.From, "to", u.current)
	if u.Notify != nil {
		_ = u.Notify(ctx, fmt.Sprintf("✅ Second Brain atualizado: %s → %s", m.From, u.current))
	}
}
