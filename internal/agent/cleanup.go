package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/database"
)

// CleanupDismiss is the action that hides a suggestion for good.
const CleanupDismiss = "dismiss"

// CleanupLabels names each kind of suggestion for people.
var CleanupLabels = map[string]string{
	database.CleanupDuplicate: "Duplicados", database.CleanupNearDuplicate: "Quase iguais", database.CleanupEmpty: "Vazios",
	database.CleanupStaleTask: "Tarefas paradas", database.CleanupLonelyPerson: "Pessoas soltas",
}

// CleanupKinds is the display order of the kinds.
var CleanupKinds = []string{database.CleanupDuplicate, database.CleanupNearDuplicate, database.CleanupStaleTask, database.CleanupEmpty, database.CleanupLonelyPerson}

// nearThreshold is the cosine similarity above which two texts are treated as the same.
func nearThreshold(model string) float64 {
	if strings.HasPrefix(model, "local:") {
		return 0.93
	}
	return 0.95
}

// SuggestCleanup runs every detector concurrently and replaces the pending suggestions.
// It returns how many suggestions of each kind are waiting for review.
func (a *Agent) SuggestCleanup(ctx context.Context) (map[string]int, error) {
	now := time.Now()
	var (
		dups                 [][]int64
		empty, stale, lonely []database.Node
		near                 []database.VectorPair
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { dups, err = a.db.DuplicateGroups(gctx, 500); return })
	g.Go(func() (err error) { empty, err = a.db.EmptyNodes(gctx, now.AddDate(0, 0, -7), 200); return })
	g.Go(func() (err error) {
		stale, err = a.db.StaleTasks(gctx, now.AddDate(0, 0, -30), now.AddDate(0, 0, -30), 200)
		return
	})
	g.Go(func() (err error) { lonely, err = a.db.LonelyAutoPersons(gctx, now.AddDate(0, 0, -14), 200); return })
	g.Go(func() error {
		from := now.AddDate(0, 0, -14) // new items against the whole brain
		ids, err := a.db.NodeIDs(gctx, database.NodeFilter{Types: []string{database.TypeNote, database.TypeArticle, database.TypeInsight, database.TypeTask}, From: &from}, 3000)
		if err != nil {
			return err
		}
		model := a.llm.EmbedModel()
		near = a.db.NearDuplicates(model, ids, nearThreshold(model))
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var list []database.CleanupSuggestion
	merged := map[int64]bool{}
	for _, grp := range dups {
		list = append(list, database.CleanupSuggestion{Kind: database.CleanupDuplicate, Action: database.ActionMerge, NodeIDs: grp,
			Reason: fmt.Sprintf("%d cópias com o mesmo título e texto (ou o mesmo link). Juntar mantém a mais antiga com as conexões de todas.", len(grp))})
		for _, id := range grp {
			merged[id] = true
		}
	}
	clusters, err := a.nearClusters(ctx, near, merged)
	if err != nil {
		return nil, err
	}
	list = append(list, clusters...)
	loc := a.cfg.Location()
	for _, n := range empty {
		list = append(list, database.CleanupSuggestion{Kind: database.CleanupEmpty, Action: database.ActionTrash, NodeIDs: []int64{n.ID},
			Reason: "Sem texto e sem conexões desde " + n.CreatedAt.In(loc).Format("02/01/2006") + "."})
	}
	for _, n := range stale {
		reason := "Aberta sem mudanças desde " + n.UpdatedAt.In(loc).Format("02/01/2006") + "."
		if n.DueAt != nil {
			reason = "Prazo venceu em " + n.DueAt.In(loc).Format("02/01/2006") + "."
		}
		list = append(list, database.CleanupSuggestion{Kind: database.CleanupStaleTask, Action: database.ActionDone, NodeIDs: []int64{n.ID}, Reason: reason + " Concluir ou apagar?"})
	}
	for _, n := range lonely {
		list = append(list, database.CleanupSuggestion{Kind: database.CleanupLonelyPerson, Action: database.ActionTrash, NodeIDs: []int64{n.ID},
			Reason: "Pessoa criada automaticamente a partir de uma única menção, sem dados de contato."})
	}
	return a.db.ReplaceCleanupSuggestions(ctx, list)
}

// nearClusters groups very similar pairs of the same type (oldest node first).
func (a *Agent) nearClusters(ctx context.Context, pairs []database.VectorPair, skip map[int64]bool) ([]database.CleanupSuggestion, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	var ids []int64
	for _, p := range pairs {
		ids = append(ids, p.A, p.B)
	}
	nodes := map[int64]database.Node{}
	for start := 0; start < len(ids); start += 400 {
		part, err := a.db.GetNodes(ctx, ids[start:min(len(ids), start+400)])
		if err != nil {
			return nil, err
		}
		for _, n := range part {
			nodes[n.ID] = n
		}
	}
	allowed := map[string]bool{database.TypeNote: true, database.TypeArticle: true, database.TypeInsight: true, database.TypeTask: true}
	generated := map[string]bool{"routine": true, MemorySource: true} // periodic reports look alike on purpose
	parent := map[int64]int64{}
	var find func(int64) int64
	find = func(x int64) int64 {
		if p, ok := parent[x]; ok && p != x {
			parent[x] = find(p)
			return parent[x]
		}
		parent[x] = x
		return x
	}
	best := map[int64]float64{}
	for _, p := range pairs {
		na, okA := nodes[p.A]
		nb, okB := nodes[p.B]
		if !okA || !okB || na.Type != nb.Type || !allowed[na.Type] || (skip[p.A] && skip[p.B]) || generated[na.Source] || generated[nb.Source] {
			continue
		}
		if na.Type == database.TypeTask && (na.Status == database.StatusDone) != (nb.Status == database.StatusDone) {
			continue
		}
		ra, rb := find(p.A), find(p.B)
		if ra != rb {
			parent[rb] = ra
		}
		best[p.A] = max(best[p.A], p.Score)
		best[p.B] = max(best[p.B], p.Score)
	}
	groups := map[int64][]database.Node{}
	for id := range parent {
		groups[find(id)] = append(groups[find(id)], nodes[id])
	}
	var out []database.CleanupSuggestion
	for _, g := range groups {
		if len(g) < 2 || len(g) > 6 {
			continue
		}
		slices.SortFunc(g, func(x, y database.Node) int {
			if c := x.CreatedAt.Compare(y.CreatedAt); c != 0 {
				return c
			}
			return int(x.ID - y.ID)
		})
		score := 0.0
		c := database.CleanupSuggestion{Kind: database.CleanupNearDuplicate, Action: database.ActionMerge}
		for _, n := range g {
			c.NodeIDs = append(c.NodeIDs, n.ID)
			score = max(score, best[n.ID])
		}
		c.Reason = fmt.Sprintf("Textos quase iguais (%.0f%% parecidos). Confira e junte se forem o mesmo assunto.", score*100)
		out = append(out, c)
	}
	slices.SortFunc(out, func(x, y database.CleanupSuggestion) int { return int(x.NodeIDs[0] - y.NodeIDs[0]) })
	return out, nil
}

// ApplyCleanup carries out a suggestion. action "" uses the suggested one; "merge" keeps
// the first node, "trash" moves to the trash (keeping the first node of a merge proposal),
// "done" completes tasks and "dismiss" hides the suggestion for good. Deletions use
// "não trazer de volta", so integrations do not recreate what was cleaned.
func (a *Agent) ApplyCleanup(ctx context.Context, id int64, action string) (string, error) {
	c, err := a.db.GetCleanup(ctx, id)
	if err != nil {
		return "", err
	}
	if c.Status != database.CleanupPending {
		return "Sugestão já resolvida.", nil
	}
	if action == "" {
		action = c.Action
	}
	existing, err := a.db.GetNodes(ctx, c.NodeIDs)
	if err != nil {
		return "", err
	}
	byID := map[int64]database.Node{}
	for _, n := range existing {
		byID[n.ID] = n
	}
	var ids []int64
	for _, nid := range c.NodeIDs { // suggestion order: the keeper first
		if _, ok := byID[nid]; ok {
			ids = append(ids, nid)
		}
	}
	var msg string
	switch action {
	case CleanupDismiss:
		return "Sugestão ignorada: não aparece de novo.", a.db.ResolveCleanup(ctx, id, database.CleanupDismissed)
	case database.ActionMerge:
		if c.Action != database.ActionMerge || len(ids) < 2 {
			return "", fmt.Errorf("não há o que juntar")
		}
		keep, err := a.db.MergeNodes(ctx, ids[0], ids[1:])
		if err != nil {
			return "", err
		}
		if _, _, err := a.db.TrashNodes(ctx, ids[1:], true); err != nil {
			return "", err
		}
		_ = a.QueueEnrich(ctx, keep.ID)
		msg = fmt.Sprintf("%d itens juntados em “%s” (as cópias foram para a lixeira).", len(ids), keep.Title)
	case database.ActionTrash:
		del := ids
		if c.Action == database.ActionMerge && len(ids) > 1 {
			del = ids[1:]
		}
		if len(del) == 0 {
			return "", fmt.Errorf("nada para apagar")
		}
		if _, _, err := a.db.TrashNodes(ctx, del, true); err != nil {
			return "", err
		}
		msg = fmt.Sprintf("%d item(ns) movido(s) para a lixeira.", len(del))
	case database.ActionDone:
		n, err := a.db.UpdateNodes(ctx, ids, func(n *database.Node) bool {
			if n.Type != database.TypeTask || n.Status == database.StatusDone {
				return false
			}
			n.Status = database.StatusDone
			return true
		})
		if err != nil {
			return "", err
		}
		msg = fmt.Sprintf("%d tarefa(s) concluída(s).", n)
	default:
		return "", fmt.Errorf("ação desconhecida: %s", action)
	}
	a.log.Info("faxina aplicada", "id", id, "kind", c.Kind, "action", action, "nodes", ids)
	return msg, a.db.ResolveCleanup(ctx, id, database.CleanupApplied)
}

// ApplyAllCleanup applies the suggested action of every pending suggestion of kind ("" = all).
func (a *Agent) ApplyAllCleanup(ctx context.Context, kind string) (int, error) {
	list, err := a.db.ListCleanup(ctx, database.CleanupPending, 5000)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range list {
		if kind != "" && c.Kind != kind {
			continue
		}
		if _, err := a.ApplyCleanup(ctx, c.ID, ""); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
