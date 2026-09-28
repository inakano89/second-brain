// Package export writes the knowledge graph as an Obsidian vault (.zip):
// Markdown files with YAML frontmatter and [[wiki-links]] for every edge.
package export

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/inakano89/second-brain/internal/database"
)

var folders = map[string]string{
	database.TypeNote: "Notas", database.TypeTask: "Tarefas", database.TypePerson: "Pessoas", database.TypeEvent: "Eventos",
	database.TypeInsight: "Insights", database.TypeArticle: "Artigos", database.TypeHealth: "Saúde",
}

// SafeName converts a title into a portable file name.
func SafeName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '#', '^', '[', ']':
			return '-'
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	s = strings.Trim(s, ". ")
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100])
	}
	if s == "" {
		s = "sem-titulo"
	}
	return s
}

func yamlStr(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ").Replace(s) + `"`
}

type edgeRef struct {
	other int64
	rel   string
	out   bool
}

// Obsidian streams a zip vault into w. Markdown rendering runs on a goroutine pool;
// a single writer goroutine serializes zip entries in id order.
func Obsidian(ctx context.Context, db *database.DB, w io.Writer, loc *time.Location) error {
	var nodes []database.Node
	if err := db.IterateNodes(ctx, func(n *database.Node) error {
		nodes = append(nodes, *n)
		return nil
	}); err != nil {
		return err
	}
	// Unique file names per node.
	names := make(map[int64]string, len(nodes))
	used := map[string]bool{}
	for _, n := range nodes {
		base := SafeName(n.Title)
		name := base
		for i := 2; used[strings.ToLower(folders[n.Type]+"/"+name)]; i++ {
			name = fmt.Sprintf("%s (%d)", base, i)
		}
		used[strings.ToLower(folders[n.Type]+"/"+name)] = true
		names[n.ID] = name
	}
	edges := map[int64][]edgeRef{}
	rows, err := db.QueryContext(ctx, `SELECT source_id, target_id, relation FROM edges`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s, t int64
		var rel string
		if err := rows.Scan(&s, &t, &rel); err != nil {
			rows.Close()
			return err
		}
		edges[s] = append(edges[s], edgeRef{t, rel, true})
		edges[t] = append(edges[t], edgeRef{s, rel, false})
	}
	rows.Close()

	type rendered struct {
		idx  int
		path string
		body string
	}
	jobs := make(chan int)
	results := make(chan rendered, 64)
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				n := &nodes[idx]
				results <- rendered{idx, folders[n.Type] + "/" + names[n.ID] + ".md", render(n, edges[n.ID], names, loc)}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := range nodes {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()

	zw := zip.NewWriter(w)
	pending := map[int]rendered{}
	next := 0
	write := func(r rendered) error {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: r.path, Method: zip.Deflate, Modified: nodes[r.idx].UpdatedAt})
		if err != nil {
			return err
		}
		_, err = io.WriteString(fw, r.body)
		return err
	}
	var werr error
	for r := range results {
		if werr != nil {
			continue // drain
		}
		pending[r.idx] = r
		for {
			p, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			if werr = write(p); werr != nil {
				break
			}
		}
	}
	if werr != nil {
		return werr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fw, err := zw.Create("README.md")
	if err != nil {
		return err
	}
	fmt.Fprintf(fw, "# Second Brain — export\n\nGerado em %s. %d notas.\n", time.Now().In(loc).Format("02/01/2006 15:04"), len(nodes))
	return zw.Close()
}

func render(n *database.Node, refs []edgeRef, names map[int64]string, loc *time.Location) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %d\nuid: %s\ntype: %s\ntitle: %s\n", n.ID, n.UID, n.Type, yamlStr(n.Title))
	if len(n.Tags) > 0 {
		b.WriteString("tags:\n")
		for _, t := range n.Tags {
			fmt.Fprintf(&b, "  - %s\n", yamlStr(t))
		}
	}
	fmt.Fprintf(&b, "created: %s\nupdated: %s\n", n.CreatedAt.In(loc).Format(time.RFC3339), n.UpdatedAt.In(loc).Format(time.RFC3339))
	if n.Source != "" {
		fmt.Fprintf(&b, "source: %s\n", yamlStr(n.Source))
	}
	if n.Status != "" {
		fmt.Fprintf(&b, "status: %s\n", n.Status)
	}
	if n.DueAt != nil {
		fmt.Fprintf(&b, "due: %s\n", n.DueAt.In(loc).Format(time.RFC3339))
	}
	if u, ok := n.Meta["url"].(string); ok && u != "" {
		fmt.Fprintf(&b, "url: %s\n", yamlStr(u))
	}
	if n.Summary != "" {
		fmt.Fprintf(&b, "summary: %s\n", yamlStr(n.Summary))
	}
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", n.Title)
	if n.Type == database.TypeTask {
		box := " "
		if n.Status == database.StatusDone {
			box = "x"
		}
		fmt.Fprintf(&b, "- [%s] %s\n\n", box, n.Title)
	}
	if n.Content != "" {
		b.WriteString(n.Content)
		b.WriteString("\n")
	}
	if len(refs) > 0 {
		byRel := map[string][]string{}
		for _, r := range refs {
			name, ok := names[r.other]
			if !ok {
				continue
			}
			label := r.rel
			if !r.out {
				label = "← " + r.rel
			}
			byRel[label] = append(byRel[label], "[["+name+"]]")
		}
		keys := make([]string, 0, len(byRel))
		for k := range byRel {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("\n## Conexões\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "- **%s:** %s\n", k, strings.Join(byRel[k], ", "))
		}
	}
	return b.String()
}
