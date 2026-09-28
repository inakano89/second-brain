package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommand(t *testing.T) {
	got := Command(`C:\Program Files\SB\second-brain.exe`, `C:\Program Files\SB\.env`)
	want := `"C:\Program Files\SB\second-brain.exe" -env "C:\Program Files\SB\.env" -background`
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func TestTrimLogAndDetachedEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	os.WriteFile(p, make([]byte, 100), 0o644)
	trimLog(p, 50)
	if st, _ := os.Stat(p); st.Size() != 0 {
		t.Fatal("log not trimmed")
	}
	t.Setenv(DetachedEnv, "1")
	if !Detached() {
		t.Fatal("Detached")
	}
}
