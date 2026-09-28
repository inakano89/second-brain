// Command modelswatch compares the providers' model documentation pages with the
// curated catalogue (internal/llm/models.json) and writes a Markdown report of
// models that look new. The "Models watch" workflow turns the report into an issue.
//
//	go run ./cmd/modelswatch -out report.md
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/llm"
)

type result struct {
	provider, url string
	ids           []string
	err           error
}

func main() {
	catalogPath := flag.String("catalog", "internal/llm/models.json", "curated catalogue")
	out := flag.String("out", "", "write the report here when there is something new (default stdout)")
	flag.Parse()

	raw, err := os.ReadFile(*catalogPath)
	if err != nil {
		fail(err)
	}
	cat, err := llm.ParseCuratedCatalog(raw)
	if err != nil {
		fail(err)
	}

	var providers []string
	for _, p := range llm.ProviderNames {
		if cat.Sources[p] != "" {
			providers = append(providers, p)
		}
	}
	results := make([]result, len(providers))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var g errgroup.Group
	for i, p := range providers {
		g.Go(func() error {
			page, err := fetch(ctx, cat.Sources[p])
			results[i] = result{provider: p, url: cat.Sources[p], ids: llm.ExtractModelIDs(p, page), err: err}
			return nil
		})
	}
	_ = g.Wait()

	var report, warnings strings.Builder
	found := false
	for _, r := range results {
		label := llm.ProviderLabel(r.provider)
		if r.err != nil || len(r.ids) == 0 {
			why := "nenhum id de modelo encontrado"
			if r.err != nil {
				why = r.err.Error()
			}
			fmt.Fprintf(&warnings, "- ⚠️ %s: não consegui ler modelos em %s (%s)\n", label, r.url, why)
			continue
		}
		cands := cat.NewModelCandidates(r.provider, r.ids)
		if len(cands) == 0 {
			continue
		}
		found = true
		fmt.Fprintf(&report, "### %s\nFonte: %s\n\n", label, r.url)
		for _, id := range cands {
			fmt.Fprintf(&report, "- [ ] `%s`\n", id)
		}
		if miss := cat.MissingFromDocs(r.provider, r.ids); len(miss) > 0 {
			fmt.Fprintf(&report, "\nNo catálogo, mas não citados na página (podem ter sido descontinuados): `%s`\n", strings.Join(miss, "`, `"))
		}
		report.WriteString("\n")
	}
	os.Stderr.WriteString(warnings.String())
	if !found {
		fmt.Fprintln(os.Stderr, "nenhum modelo novo encontrado")
		return
	}
	body := fmt.Sprintf(`Modelos que aparecem nas documentações oficiais e ainda não estão em `+"`internal/llm/models.json`"+` (revisão %d, %s).

%s%s
**Como atualizar:** edite `+"`internal/llm/models.json`"+` (adicione/remova modelos, ajuste o `+"`default`"+` e os preços), **aumente o `+"`revision`"+`**, rode `+"`make check`"+` e abra um Pull Request. Depois do merge em `+"`main`"+`, todas as instalações aplicam o novo catálogo em até 24 h, sem nova release.
Para silenciar um item que não deve entrar no catálogo, adicione-o a `+"`watch_ignore`"+`.

_Gerado automaticamente pelo workflow Models watch._
`, cat.Revision, cat.Updated, report.String(), warnings.String())
	if *out == "" {
		fmt.Print(body)
		return
	}
	if err := os.WriteFile(*out, []byte(body), 0o644); err != nil {
		fail(err)
	}
}

func fetch(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "second-brain-modelswatch (+https://github.com/inakano89/second-brain)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return string(b), err
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "modelswatch:", err)
	os.Exit(1)
}
