// Package desktop adapts the server to personal computers that are not on 24/7
// (Windows): start at login without a console window, a single instance, and
// opening the browser.
package desktop

import (
	"os"
	"strings"
)

// DetachedEnv marks a process relaunched in the background (no console window).
const DetachedEnv = "SB_DETACHED"

// Detached reports whether this process is the background copy started by Detach.
func Detached() bool { return os.Getenv(DetachedEnv) == "1" }

// Command is the login command line: the binary, its .env and -background.
func Command(exe, envPath string) string {
	return quote(exe) + " -env " + quote(envPath) + " -background"
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, "") + `"` }

// trimLog keeps the background log file from growing without bound.
func trimLog(path string, max int64) {
	if st, err := os.Stat(path); err == nil && st.Size() > max {
		_ = os.Truncate(path, 0)
	}
}
