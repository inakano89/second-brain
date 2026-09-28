package agent

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
)

// SearchResult is a hybrid search hit.
type SearchResult struct {
	Node    database.Node `json:"node"`
	Score   float64       `json:"score"`
	Via     string        `json:"via"` // fts | vector | hybrid
	Snippet string        `json:"snippet"`
}

const rrfK = 60.0

// Search runs FTS5 (BM25) and vector similarity concurrently and fuses them with RRF.
func (a *Agent) Search(ctx context.Context, q string, f database.NodeFilter, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	q = strings.TrimSpace(q)
	if q == "" {
		f.Limit = limit
		nodes, err := a.db.ListNodes(ctx, f)
		if err != nil {
			return nil, err
		}
		out := make([]SearchResult, len(nodes))
		for i, n := range nodes {
			out[i] = SearchResult{Node: n, Via: "recent", Snippet: snippet(&n, "")}
		}
		return out, nil
	}

	var (
		wg        sync.WaitGroup
		ftsHits   []database.ScoredNode
		ftsErr    error
		vecNodes  []database.Node
		vecScores map[int64]float64
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		ftsHits, ftsErr = a.db.SearchFTS(ctx, q, f, limit*3)
	}()
	go func() {
		defer wg.Done()
		vec, model, err := a.queryVector(ctx, q)
		if err != nil {
			a.log.Debug("busca vetorial indisponível", "err", err)
			return
		}
		hits := a.db.VectorSearch(model, vec, limit*4, nil)
		ids := make([]int64, 0, len(hits))
		vecScores = make(map[int64]float64, len(hits))
		for _, h := range hits {
			ids = append(ids, h.ID)
			vecScores[h.ID] = h.Score
		}
		nodes, err := a.db.GetNodes(ctx, ids)
		if err != nil {
			return
		}
		sort.Slice(nodes, func(i, j int) bool { return vecScores[nodes[i].ID] > vecScores[nodes[j].ID] })
		for _, n := range nodes {
			if f.Matches(&n) {
				vecNodes = append(vecNodes, n)
			}
		}
	}()
	wg.Wait()
	if ftsErr != nil && len(vecNodes) == 0 {
		return nil, ftsErr
	}

	type acc struct {
		node  database.Node
		score float64
		fts   bool
		vec   bool
	}
	fused := map[int64]*acc{}
	for rank, h := range ftsHits {
		fused[h.ID] = &acc{node: h.Node, score: 1 / (rrfK + float64(rank+1)), fts: true}
	}
	minSim := 0.2
	if strings.HasPrefix(a.llm.EmbedModel(), "local:") {
		minSim = 0.12
	}
	for rank, n := range vecNodes {
		if vecScores[n.ID] < minSim {
			continue
		}
		s := 1 / (rrfK + float64(rank+1))
		if x, ok := fused[n.ID]; ok {
			x.score += s
			x.vec = true
		} else {
			fused[n.ID] = &acc{node: n, score: s, vec: true}
		}
	}
	out := make([]SearchResult, 0, len(fused))
	for _, x := range fused {
		via := "fts"
		switch {
		case x.fts && x.vec:
			via = "hybrid"
		case x.vec:
			via = "vector"
		}
		out = append(out, SearchResult{Node: x.node, Score: x.score, Via: via, Snippet: snippet(&x.node, q)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (a *Agent) queryVector(ctx context.Context, q string) ([]float32, string, error) {
	model := a.llm.EmbedModel()
	key := model + "\x00" + strings.ToLower(q)
	a.qcacheMu.Lock()
	if v, ok := a.qcache[key]; ok {
		a.qcacheMu.Unlock()
		return v, model, nil
	}
	a.qcacheMu.Unlock()
	vecs, m, err := a.llm.Embed(ctx, []string{q})
	if err != nil {
		// Degrade to the local embedder only if the index was built with it.
		return nil, "", err
	}
	a.qcacheMu.Lock()
	a.qcache[key] = vecs[0]
	a.qorder = append(a.qorder, key)
	if len(a.qorder) > 256 {
		delete(a.qcache, a.qorder[0])
		a.qorder = a.qorder[1:]
	}
	a.qcacheMu.Unlock()
	return vecs[0], m, nil
}

func snippet(n *database.Node, q string) string {
	src := n.Summary
	if src == "" {
		src = n.Content
	}
	if q != "" && n.Content != "" {
		lc := strings.ToLower(n.Content)
		for _, t := range llm.Tokenize(q) {
			if i := strings.Index(lc, t); i >= 0 {
				start := max(0, i-80)
				for start > 0 && !isRuneStart(n.Content[start]) {
					start--
				}
				src = n.Content[start:]
				if start > 0 {
					src = "…" + src
				}
				break
			}
		}
	}
	return extract.Truncate(strings.Join(strings.Fields(src), " "), 220)
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
