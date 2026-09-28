package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateExample = flag.Bool("update-env-example", false, "rewrite .env.example from the schema")

func renderExample() string {
	var b strings.Builder
	b.WriteString("# Second Brain — exemplo de configuração (.env)\n")
	b.WriteString("# O assistente /setup gera este arquivo automaticamente no primeiro acesso.\n")
	b.WriteString("# Precedência: valor no .env > variável de ambiente > padrão.\n\n")
	var groups []string
	byGroup := map[string][]Field{}
	for _, f := range Schema {
		if _, ok := byGroup[f.Group]; !ok {
			groups = append(groups, f.Group)
		}
		byGroup[f.Group] = append(byGroup[f.Group], f)
	}
	for _, g := range groups {
		fmt.Fprintf(&b, "\n# ===== %s =====\n", g)
		for _, f := range byGroup[g] {
			switch {
			case f.Label != "":
				line := "# " + f.Label
				if f.Help != "" {
					line += " — " + f.Help
				}
				b.WriteString(line + "\n")
			case f.Secret:
				b.WriteString("# gerado pelo setup\n")
			}
			fmt.Fprintf(&b, "%s=%s\n", f.Key, quote(f.Default))
		}
	}
	return b.String()
}

// TestEnvExampleUpToDate keeps .env.example in sync with the schema.
// Regenerate with: go test ./internal/config -run EnvExample -update-env-example
func TestEnvExampleUpToDate(t *testing.T) {
	path := filepath.Join("..", "..", ".env.example")
	want := renderExample()
	if *updateExample {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatal(".env.example desatualizado: rode go test ./internal/config -run EnvExample -update-env-example")
	}
}
