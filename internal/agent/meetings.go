package agent

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sync/errgroup"
	"golang.org/x/text/unicode/norm"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
)

// MetaActionsAt records when the meeting-tasks routine read a node.
const MetaActionsAt = "actions_at"

// meetingRe recognises meetings, calls and their transcripts or minutes by title.
var meetingRe = regexp.MustCompile(`(?i)(reuni[ãa]o|reuni[õo]es|\bmeeting|\bcall\b|\b1:1\b|one[- ]on[- ]one|transcri[çc][ãa]o|transcript|\bata\b|\bminutes\b|\bdaily\b|stand-?up|kick-?off|retrospectiva|\bretro\b|entrevista|alinhamento|anota[çc][õo]es do gemini|notes by gemini)`)

var meetingTags = map[string]bool{"reuniao": true, "reunioes": true, "meeting": true, "transcricao": true, "transcript": true, "ata": true, "call": true}

// skipSources never hold the user's own meetings.
var skipSources = map[string]bool{"routine": true, "memory": true, "rss": true, "newsletter": true, "youtube": true, "health": true, "webhook": true}

// IsMeeting reports whether n looks like meeting notes, minutes or a transcript worth
// reading for action items.
func IsMeeting(n *database.Node) bool {
	if n.Type == database.TypeTask || n.Type == database.TypeHealth || n.Type == database.TypePerson || skipSources[n.Source] {
		return false
	}
	size := len([]rune(strings.TrimSpace(n.Content)))
	if size < 200 {
		return false
	}
	for _, t := range n.Tags {
		if meetingTags[t] {
			return true
		}
	}
	switch {
	case meetingRe.MatchString(n.Title):
		return true
	case n.Type == database.TypeEvent: // events only count when someone wrote notes in them
		return size >= 400
	case slices.Contains(n.Tags, "voz"): // long voice memo: usually a recorded conversation
		return size >= 600
	}
	return false
}

// foldTitle normalises a task title for duplicate detection.
func foldTitle(s string) string {
	var b strings.Builder
	space := false
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			space = false
		case !space && b.Len() > 0:
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func shortHash(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:6])
}

// similarTitle is true when two folded titles are equal or one contains the other.
func similarTitle(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || (len(a) > 12 && strings.Contains(b, a)) || (len(b) > 12 && strings.Contains(a, b))
}

// ActionsReport summarises a meeting-tasks run.
type ActionsReport struct {
	Scanned int             // meetings read by the LLM
	Created []database.Node // tasks created
}

type meetingTask struct {
	SourceID int64  `json:"source_id"`
	Title    string `json:"title"`
	Owner    string `json:"owner"`
	Due      string `json:"due"`
	Context  string `json:"context"`
}

const actionsSystem = `Você extrai AÇÕES COMBINADAS de reuniões (atas, transcrições, anotações de chamadas).
Responda SOMENTE JSON: {"tasks":[{"source_id":123,"title":"verbo no infinitivo + objeto, máx. 100 caracteres","owner":"me ou o nome de quem ficou responsável","due":"YYYY-MM-DD ou vazio","context":"1 frase: o que foi combinado e por quê"}]}
Regras:
- Só compromissos concretos e ainda pendentes; ignore ideias soltas, opiniões e o que já foi feito na própria reunião.
- owner "me" quando a ação é do dono do cérebro ou quando não fica claro que é de outra pessoa.
- Converta prazos relativos ("sexta", "semana que vem") usando a data da reunião.
- Não repita tarefas que já estão em <tarefas_abertas>. No máximo 8 por reunião.
- source_id é o id da reunião de onde a ação saiu. Mantenha o idioma original.`

// ExtractMeetingTasks reads meetings changed in [from, to) that have no extracted tasks yet
// and creates the action items agreed in them (tasks linked to the meeting). Without an
// LLM it does nothing.
func (a *Agent) ExtractMeetingTasks(ctx context.Context, from, to time.Time) (*ActionsReport, error) {
	rep := &ActionsReport{}
	if !a.llm.Enabled() {
		return rep, nil
	}
	changed, err := a.db.ChangedSince(ctx, []string{database.TypeNote, database.TypeEvent, database.TypeInsight, database.TypeArticle}, from, to, 1000)
	if err != nil {
		return nil, err
	}
	var meetings []database.Node
	for i := len(changed) - 1; i >= 0 && len(meetings) < 20; i-- { // newest first
		n := changed[i]
		if !IsMeeting(&n) {
			continue
		}
		if at, ok := n.Meta[MetaActionsAt].(string); ok {
			if t, err := time.Parse(time.RFC3339, at); err == nil && !n.UpdatedAt.After(t) {
				continue // already read and unchanged
			}
		}
		if c, err := a.db.DerivedTaskCount(ctx, n.ID); err != nil {
			return nil, err
		} else if c > 0 {
			continue // tasks already extracted on enrichment or e-mail triage
		}
		meetings = append(meetings, n)
	}
	if len(meetings) == 0 {
		return rep, nil
	}
	open, err := a.db.OpenTasks(ctx, 200)
	if err != nil {
		return nil, err
	}
	known := make([]string, 0, len(open))
	var openList strings.Builder
	for i, t := range open {
		known = append(known, foldTitle(t.Title))
		if i < 80 {
			fmt.Fprintf(&openList, "- %s\n", t.Title)
		}
	}

	loc := a.cfg.Location()
	var (
		mu    sync.Mutex
		found []meetingTask
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(3)
	for start := 0; start < len(meetings); start += 4 {
		batch := meetings[start:min(len(meetings), start+4)]
		g.Go(func() error {
			var b strings.Builder
			fmt.Fprintf(&b, "Hoje: %s\n\n<tarefas_abertas>\n%s</tarefas_abertas>\n", time.Now().In(loc).Format("2006-01-02 (Monday)"), openList.String())
			ids := map[int64]bool{}
			for _, m := range batch {
				ids[m.ID] = true
				when := m.CreatedAt
				if m.DueAt != nil {
					when = *m.DueAt
				}
				fmt.Fprintf(&b, "\n<reuniao source_id=%d data=%s>\n# %s\n%s\n</reuniao>\n", m.ID, when.In(loc).Format("2006-01-02"), m.Title, extract.Truncate(m.Content, 9000))
			}
			var out struct {
				Tasks []meetingTask `json:"tasks"`
			}
			err := a.llm.CompleteJSON(gctx, "", llm.Request{Purpose: "actions", System: actionsSystem, MaxTokens: 3000, Effort: "low",
				Messages: []llm.Message{{Role: llm.RoleUser, Content: b.String()}}}, &out)
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			for _, t := range out.Tasks {
				if ids[t.SourceID] {
					found = append(found, t)
				}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	rep.Scanned = len(meetings)

	byID := map[int64]database.Node{}
	for _, m := range meetings {
		byID[m.ID] = m
	}
	perMeeting := map[int64]int{}
	for _, t := range found {
		title := strings.TrimSpace(t.Title)
		owner := strings.TrimSpace(t.Owner)
		tags := []string{"reuniao", "ia"}
		if owner != "" && !strings.EqualFold(owner, "me") && !strings.EqualFold(owner, "eu") {
			title = "Aguardando " + owner + ": " + title
			tags = append(tags, "aguardando")
		}
		f := foldTitle(title)
		if f == "" || perMeeting[t.SourceID] >= 8 || slices.ContainsFunc(known, func(k string) bool { return similarTitle(k, f) }) {
			continue
		}
		src := byID[t.SourceID]
		var due *time.Time
		if d, err := time.ParseInLocation("2006-01-02", t.Due, loc); err == nil {
			due = &d
		}
		content := strings.TrimSpace(t.Context + "\n\nOrigem: [[" + src.Title + "]]")
		task, _, err := a.Ingest(ctx, IngestInput{
			Type: database.TypeTask, Title: extract.Truncate(title, 160), Content: content, Source: "agent",
			SourceRef: fmt.Sprintf("meeting:%d:%s", src.ID, shortHash(f)), DueAt: due, Tags: tags,
			Meta: map[string]any{"from_node": src.ID, "owner": owner, "enriched": true}, Enrich: true,
		})
		if errors.Is(err, database.ErrDeleted) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := a.db.AddEdge(ctx, task.ID, src.ID, "derived_from", 1); err != nil {
			return nil, err
		}
		known = append(known, f)
		perMeeting[t.SourceID]++
		rep.Created = append(rep.Created, *task)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, m := range meetings {
		if err := a.db.SetMetaKey(ctx, m.ID, MetaActionsAt, stamp); err != nil {
			return nil, err
		}
	}
	a.log.Info("tarefas de reuniões", "meetings", rep.Scanned, "tasks", len(rep.Created))
	return rep, nil
}

// TaskOrigin is a task extracted by the AI with the note, meeting or e-mail it came from.
type TaskOrigin struct {
	Task   database.Node
	Origin *database.Node
}

// NewAITasks lists open tasks created since since by the AI (meetings, notes, e-mails),
// newest first, with where each one came from.
func (a *Agent) NewAITasks(ctx context.Context, since time.Time, limit int) ([]TaskOrigin, error) {
	tasks, err := a.db.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypeTask}, Status: database.StatusOpen,
		Sources: []string{"agent", "gmail"}, From: &since, Limit: limit})
	if err != nil || len(tasks) == 0 {
		return nil, err
	}
	ids := make([]int64, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	origins, err := a.db.DerivedOrigins(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]TaskOrigin, len(tasks))
	for i, t := range tasks {
		out[i] = TaskOrigin{Task: t}
		if o, ok := origins[t.ID]; ok {
			out[i].Origin = &o
		}
	}
	return out, nil
}
