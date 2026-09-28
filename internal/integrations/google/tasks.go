package google

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
)

const tasksAPI = "https://tasks.googleapis.com/tasks/v1/"

type gTaskList struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type gTask struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Notes       string `json:"notes"`
	Status      string `json:"status"`
	Due         string `json:"due"`
	Updated     string `json:"updated"`
	Parent      string `json:"parent"`
	WebViewLink string `json:"webViewLink"`
	Deleted     bool   `json:"deleted"`
}

// dueDate converts Tasks' date-only due (midnight UTC) into local midnight.
func dueDate(s string, loc *time.Location) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	return &d
}

func (c *Client) taskLists(ctx context.Context) ([]gTaskList, error) {
	var out []gTaskList
	v := url.Values{"maxResults": {"100"}}
	for page := 0; page < 10; page++ {
		var resp struct {
			Items         []gTaskList `json:"items"`
			NextPageToken string      `json:"nextPageToken"`
		}
		if err := c.do(ctx, http.MethodGet, tasksAPI+"users/@me/lists?"+v.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Items...)
		if resp.NextPageToken == "" {
			break
		}
		v.Set("pageToken", resp.NextPageToken)
	}
	return out, nil
}

func (c *Client) tasks(ctx context.Context, list string, updatedMin string) ([]gTask, error) {
	v := url.Values{"maxResults": {"100"}, "showCompleted": {"true"}, "showHidden": {"true"}, "showDeleted": {"true"}}
	if updatedMin != "" {
		v.Set("updatedMin", updatedMin)
	}
	var out []gTask
	for page := 0; page < 50; page++ {
		var resp struct {
			Items         []gTask `json:"items"`
			NextPageToken string  `json:"nextPageToken"`
		}
		if err := c.do(ctx, http.MethodGet, tasksAPI+"lists/"+url.PathEscape(list)+"/tasks?"+v.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Items...)
		if resp.NextPageToken == "" {
			break
		}
		v.Set("pageToken", resp.NextPageToken)
	}
	return out, nil
}

const tasksCursorKey = "google.tasks.updated_min"

// SyncTasks mirrors Google Tasks as task nodes (Google is the source of truth when a task changes there).
func (s *Syncer) SyncTasks(ctx context.Context) (int, error) {
	if !s.g.Can(agent.GoogleTasks) {
		return 0, nil
	}
	db := s.ag.DB()
	started := time.Now().UTC()
	updatedMin, _, err := db.KVGet(ctx, tasksCursorKey)
	if err != nil {
		return 0, err
	}
	lists, err := s.g.taskLists(ctx)
	if err != nil {
		return 0, err
	}
	var mu sync.Mutex
	count := 0
	parents := map[string]string{}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(3)
	for _, l := range lists {
		g.Go(func() error {
			items, err := s.g.tasks(gctx, l.ID, updatedMin)
			if err != nil {
				return err
			}
			for _, t := range items {
				changed, err := s.upsertTask(gctx, l, t)
				if err != nil {
					return err
				}
				mu.Lock()
				if changed {
					count++
				}
				if t.Parent != "" && !t.Deleted {
					parents[t.ID] = t.Parent
				}
				mu.Unlock()
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return count, err
	}
	for child, parent := range parents {
		cn, err1 := db.GetNodeBySource(ctx, "gtasks", child)
		pn, err2 := db.GetNodeBySource(ctx, "gtasks", parent)
		if err1 == nil && err2 == nil {
			_ = db.AddEdge(ctx, cn.ID, pn.ID, "part_of", 1)
		}
	}
	return count, db.KVSet(ctx, tasksCursorKey, started.Add(-time.Minute).Format(time.RFC3339))
}

func (s *Syncer) upsertTask(ctx context.Context, l gTaskList, t gTask) (bool, error) {
	db := s.ag.DB()
	existing, err := db.GetNodeBySource(ctx, "gtasks", t.ID)
	if err != nil && !errors.Is(err, database.ErrNotFound) {
		return false, err
	}
	if t.Deleted {
		if existing != nil {
			return true, db.DeleteNode(ctx, existing.ID)
		}
		return false, nil
	}
	if strings.TrimSpace(t.Title) == "" {
		return false, nil
	}
	if existing != nil {
		if u, _ := existing.Meta["updated"].(string); u == t.Updated {
			return false, nil
		}
	}
	status := database.StatusOpen
	if t.Status == "completed" {
		status = database.StatusDone
	}
	content := strings.TrimSpace(t.Notes + "\n\nLista: " + l.Title)
	if t.WebViewLink != "" {
		content += "\n" + t.WebViewLink
	}
	_, _, err = s.ag.Ingest(ctx, agent.IngestInput{
		Type: database.TypeTask, Title: t.Title, Content: content, Status: status, DueAt: dueDate(t.Due, s.g.cfg.Location()),
		Tags: []string{"google-tasks", l.Title}, Source: "gtasks", SourceRef: t.ID, Enrich: true,
		Meta: map[string]any{"list": l.Title, "updated": t.Updated, "link": t.WebViewLink, "enriched": true},
	})
	if errors.Is(err, database.ErrDeleted) {
		return false, nil
	}
	return err == nil, err
}
