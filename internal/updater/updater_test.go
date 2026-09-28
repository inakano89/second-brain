package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
)

func TestCompareAndRelease(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0}, {"v1.10.0", "v1.9.9", 1}, {"1.0.0", "v1.0.1", -1},
		{"v1.0.0-rc.1", "v1.0.0", -1}, {"v1.0.0-rc.2", "v1.0.0-rc.10", -1}, {"v1.0.0-beta", "v1.0.0-alpha", 1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
	for v, want := range map[string]bool{"v1.2.3": true, "v1.2.3-rc.1": true, "dev": false, "v1.2.3-4-gabc1234": false, "v1.2.3-dirty": false} {
		if IsRelease(v) != want {
			t.Errorf("IsRelease(%s) != %v", v, want)
		}
	}
}

func TestInstallFromMockGitHubWithSignature(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as fake binary")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "second-brain")
	os.WriteFile(exe, []byte("#!/bin/sh\necho second-brain v1.0.0\n"), 0o755)
	newBin := []byte("#!/bin/sh\necho second-brain v1.1.0\n")
	sum := sha256.Sum256(newBin)
	sums := []byte(fmt.Sprintf("%s  %s\n%s  other-file\n", hex.EncodeToString(sum[:]), AssetName(), strings.Repeat("0", 64)))
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums))

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/latest":
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v1.1.0", "html_url": "https://github.com/o/r/releases/v1.1.0", "body": "## Novidades",
				"assets": []map[string]any{
					{"name": AssetName(), "browser_download_url": srv.URL + "/dl/bin", "size": len(newBin)},
					{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/dl/sums"},
					{"name": "SHA256SUMS.sig", "browser_download_url": srv.URL + "/dl/sig"},
				},
			})
		case "/dl/bin":
			w.Write(newBin)
		case "/dl/sums":
			w.Write(sums)
		case "/dl/sig":
			io.WriteString(w, sig)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	os.WriteFile(filepath.Join(dir, ".env"), []byte("UPDATE_REPO=o/r\nDATA_DIR=./data\n"), 0o600)
	cfg, _ := config.Load(filepath.Join(dir, ".env"))
	db, err := database.Open(filepath.Join(dir, "data", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	u := New(cfg, db, log, "v1.0.0", base64.StdEncoding.EncodeToString(pub), exe)
	u.apiBase, u.inCtr = srv.URL, func() bool { return false }
	restarted := false
	u.RequestRestart = func() { restarted = true }

	st, err := u.Check(context.Background())
	if err != nil || !st.Available || st.Latest != "v1.1.0" || !st.Supported || !st.Signed {
		t.Fatalf("check: %+v %v", st, err)
	}
	if err := u.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != string(newBin) || !restarted {
		t.Fatalf("binary not swapped (restart=%v)", restarted)
	}
	if _, err := os.Stat(exe + ".old"); err != nil {
		t.Fatal("previous binary not kept")
	}
	if snaps, _ := filepath.Glob(filepath.Join(dir, "data", "backups", "pre-update-v1.0.0.db")); len(snaps) != 1 {
		t.Fatal("pre-update snapshot missing")
	}

	// Tampered signature must be rejected.
	os.WriteFile(exe, []byte("#!/bin/sh\necho second-brain v1.0.0\n"), 0o755)
	sig = base64.StdEncoding.EncodeToString(make([]byte, 64))
	u2 := New(cfg, db, log, "v1.0.0", base64.StdEncoding.EncodeToString(pub), exe)
	u2.apiBase, u2.inCtr = srv.URL, func() bool { return false }
	if err := u2.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "assinatura") {
		t.Fatalf("tampered signature accepted: %v", err)
	}

	// Rollback after repeated failed boots.
	os.WriteFile(exe, newBin, 0o755)
	os.WriteFile(exe+".old", []byte("old"), 0o755)
	writeMarker(exe, marker{From: "v1.0.0", To: "v1.1.0"})
	logf := func(string, ...any) {}
	for i := 0; i < maxBootAttempts; i++ {
		if recoverStep(exe, logf) {
			t.Fatal("rolled back too early")
		}
	}
	if !recoverStep(exe, logf) {
		t.Fatal("expected rollback")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" || !isSkipped(exe, "v1.1.0") {
		t.Fatal("rollback did not restore previous binary / skip bad version")
	}
	u3 := New(cfg, db, log, "v1.0.0", "", exe)
	if u3.newer("v1.1.0") {
		t.Fatal("skipped version offered again")
	}
}
