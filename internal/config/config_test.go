package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadUpdatePreservesComments(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# comentário\nBRAIN_NAME=\"Meu Cérebro\"\nexport HTTP_PORT=9090\nX='raw # value'\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Get("BRAIN_NAME") != "Meu Cérebro" || c.GetInt("HTTP_PORT", 0) != 9090 || c.Get("X") != "raw # value" {
		t.Fatalf("parse: %q %d %q", c.Get("BRAIN_NAME"), c.GetInt("HTTP_PORT", 0), c.Get("X"))
	}
	if c.Get("TIMEZONE") != "America/Sao_Paulo" {
		t.Fatal("default not applied")
	}
	if err := c.Update(map[string]string{"TELEGRAM_BOT_TOKEN": "a b\"c", "HTTP_PORT": "8081"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if !strings.HasPrefix(s, "# comentário\n") || !strings.Contains(s, "HTTP_PORT=8081") {
		t.Fatalf("render:\n%s", s)
	}
	c2, _ := Load(p)
	if c2.Get("TELEGRAM_BOT_TOKEN") != "a b\"c" {
		t.Fatalf("roundtrip: %q", c2.Get("TELEGRAM_BOT_TOKEN"))
	}
	if _, err := os.Stat(p + ".lock"); !os.IsNotExist(err) {
		t.Fatal("lock not released")
	}
}
