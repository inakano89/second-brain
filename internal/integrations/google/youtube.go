package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
)

const youtubeAPI = "https://www.googleapis.com/youtube/v3/"

type ytItem struct {
	ID      string `json:"id"`
	Snippet struct {
		Title                  string `json:"title"`
		Description            string `json:"description"`
		ChannelTitle           string `json:"channelTitle"`
		ChannelID              string `json:"channelId"`
		PublishedAt            string `json:"publishedAt"`
		VideoOwnerChannelTitle string `json:"videoOwnerChannelTitle"`
		ResourceID             struct {
			VideoID   string `json:"videoId"`
			ChannelID string `json:"channelId"`
		} `json:"resourceId"`
	} `json:"snippet"`
	ContentDetails struct {
		ItemCount int `json:"itemCount"`
	} `json:"contentDetails"`
}

// ytList pages through a YouTube list endpoint; fn returns false to stop early.
func (c *Client) ytList(ctx context.Context, endpoint string, v url.Values, maxPages int, fn func([]ytItem) bool) error {
	v.Set("maxResults", "50")
	for page := 0; page < maxPages; page++ {
		var resp struct {
			Items         []ytItem `json:"items"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := c.do(ctx, http.MethodGet, youtubeAPI+endpoint+"?"+v.Encode(), nil, &resp); err != nil {
			return err
		}
		if !fn(resp.Items) || resp.NextPageToken == "" {
			return nil
		}
		v.Set("pageToken", resp.NextPageToken)
	}
	return nil
}

// SyncYouTube imports liked videos, subscriptions and playlists concurrently.
// (Watch history has no API: it comes from Google Takeout.)
func (s *Syncer) SyncYouTube(ctx context.Context) (int, error) {
	if !s.g.Can(agent.GoogleYouTube) {
		return 0, nil
	}
	var counts [3]int
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { counts[0], err = s.syncLikes(gctx); return })
	g.Go(func() (err error) { counts[1], err = s.syncSubscriptions(gctx); return })
	g.Go(func() (err error) { counts[2], err = s.syncPlaylists(gctx); return })
	err := g.Wait()
	return counts[0] + counts[1] + counts[2], err
}

func (s *Syncer) syncLikes(ctx context.Context) (int, error) {
	db := s.ag.DB()
	const doneKey = "google.youtube.likes_backfilled"
	pages := 2
	if _, ok, _ := db.KVGet(ctx, doneKey); !ok {
		pages = 20 // first run: up to 1000 likes
	}
	var ferr error
	count := 0
	err := s.g.ytList(ctx, "videos", url.Values{"myRating": {"like"}, "part": {"snippet"}}, pages, func(items []ytItem) bool {
		known := 0
		for _, it := range items {
			ref := "like:" + it.ID
			if _, err := db.GetNodeBySource(ctx, "youtube", ref); err == nil {
				known++
				continue
			} else if !errors.Is(err, database.ErrNotFound) {
				ferr = err
				return false
			}
			link := "https://www.youtube.com/watch?v=" + it.ID
			published, _ := time.Parse(time.RFC3339, it.Snippet.PublishedAt)
			content := fmt.Sprintf("**Canal:** %s\n**Publicado:** %s\n**Link:** %s\n\n%s", it.Snippet.ChannelTitle,
				published.In(s.g.cfg.Location()).Format("02/01/2006"), link, extract.Truncate(it.Snippet.Description, 3000))
			if _, _, err := s.ag.Ingest(ctx, agent.IngestInput{
				Type: database.TypeArticle, Title: it.Snippet.Title, Content: content, Tags: []string{"youtube", "curtido"},
				Source: "youtube", SourceRef: ref, Enrich: true,
				Meta: map[string]any{"link": link, "channel": it.Snippet.ChannelTitle, "enriched": true},
			}); errors.Is(err, database.ErrDeleted) {
				known++
				continue
			} else if err != nil {
				ferr = err
				return false
			}
			count++
		}
		return known < len(items) // a page already imported means we caught up
	})
	if err = errors.Join(err, ferr); err == nil {
		_ = db.KVSet(ctx, doneKey, "1")
	}
	return count, err
}

func (s *Syncer) syncSubscriptions(ctx context.Context) (int, error) {
	var lines []string
	err := s.g.ytList(ctx, "subscriptions", url.Values{"mine": {"true"}, "part": {"snippet"}, "order": {"alphabetical"}}, 40, func(items []ytItem) bool {
		for _, it := range items {
			line := fmt.Sprintf("- [%s](https://www.youtube.com/channel/%s)", it.Snippet.Title, it.Snippet.ResourceID.ChannelID)
			if d := strings.TrimSpace(strings.ReplaceAll(it.Snippet.Description, "\n", " ")); d != "" {
				line += " — " + extract.Truncate(d, 160)
			}
			lines = append(lines, line)
		}
		return true
	})
	if err != nil || len(lines) == 0 {
		return 0, err
	}
	body := fmt.Sprintf("%d canais inscritos no YouTube.\n\n%s", len(lines), strings.Join(lines, "\n"))
	return s.upsertSummary(ctx, "youtube", "subscriptions", "YouTube — canais inscritos", body, []string{"youtube", "inscricoes"})
}

func (s *Syncer) syncPlaylists(ctx context.Context) (int, error) {
	var lists []ytItem
	err := s.g.ytList(ctx, "playlists", url.Values{"mine": {"true"}, "part": {"snippet,contentDetails"}}, 4, func(items []ytItem) bool {
		lists = append(lists, items...)
		return true
	})
	if err != nil || len(lists) == 0 {
		return 0, err
	}
	lists = lists[:min(len(lists), 25)]
	sections := make([]string, len(lists))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i, pl := range lists {
		g.Go(func() error {
			var b strings.Builder
			fmt.Fprintf(&b, "## [%s](https://www.youtube.com/playlist?list=%s) (%d vídeos)\n", pl.Snippet.Title, pl.ID, pl.ContentDetails.ItemCount)
			if d := strings.TrimSpace(pl.Snippet.Description); d != "" {
				b.WriteString(extract.Truncate(d, 300) + "\n")
			}
			err := s.g.ytList(gctx, "playlistItems", url.Values{"playlistId": {pl.ID}, "part": {"snippet"}}, 1, func(items []ytItem) bool {
				for _, it := range items {
					fmt.Fprintf(&b, "- %s", it.Snippet.Title)
					if ch := it.Snippet.VideoOwnerChannelTitle; ch != "" {
						b.WriteString(" — " + ch)
					}
					b.WriteString("\n")
				}
				return false
			})
			sections[i] = b.String()
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return 0, err
	}
	return s.upsertSummary(ctx, "youtube", "playlists", "YouTube — minhas playlists", strings.Join(sections, "\n"), []string{"youtube", "playlists"})
}

// upsertSummary stores a single aggregated note, skipping unchanged content.
func (s *Syncer) upsertSummary(ctx context.Context, source, ref, title, body string, tags []string) (int, error) {
	hash := hashOf(title, body)
	if s.unchanged(ctx, source, ref, hash) {
		return 0, nil
	}
	_, _, err := s.ag.Ingest(ctx, agent.IngestInput{
		Type: database.TypeNote, Title: title, Content: body, Summary: extract.FirstLine(body), Tags: tags,
		Source: source, SourceRef: ref, Meta: map[string]any{"hash": hash, "enriched": true}, Enrich: true,
	})
	if errors.Is(err, database.ErrDeleted) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return 1, nil
}
