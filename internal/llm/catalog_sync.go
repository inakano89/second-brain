package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/inakano89/second-brain/internal/config"
)

// CatalogStore persists the last applied catalogue (the database KV table).
type CatalogStore interface {
	KVGet(ctx context.Context, key string) (string, bool, error)
	KVSet(ctx context.Context, key, value string) error
}

const catalogKVKey = "llm.catalog"

// CatalogStatus is what the /models page shows about the catalogue.
type CatalogStatus struct {
	Revision  int
	Updated   string
	Source    string
	CheckedAt time.Time
	Changes   []string
	Err       string
}

// CatalogSync keeps the .env model settings aligned with the curated catalogue:
// the copy embedded in the binary and, daily, the one on the repository's main
// branch (so a new model reaches every install without a release).
type CatalogSync struct {
	cfg    *config.Config
	llm    *Manager
	store  CatalogStore
	client *http.Client
	log    *slog.Logger
	// URL returns the remote catalogue address ("" disables remote fetches).
	URL func() string
	// Notify, when set, reports applied changes (Telegram).
	Notify func(ctx context.Context, text string) error

	mu     sync.Mutex
	status CatalogStatus
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// NewCatalogSync builds the synchroniser.
func NewCatalogSync(cfg *config.Config, m *Manager, store CatalogStore, log *slog.Logger) *CatalogSync {
	s := &CatalogSync{cfg: cfg, llm: m, store: store, client: &http.Client{Timeout: 30 * time.Second}, log: log.With("component", "models")}
	s.URL = func() string {
		repo := strings.TrimSpace(cfg.Get("UPDATE_REPO"))
		if !repoRe.MatchString(repo) {
			return ""
		}
		return "https://raw.githubusercontent.com/" + repo + "/main/internal/llm/models.json"
	}
	return s
}

func (s *CatalogSync) applied(ctx context.Context) *CuratedCatalog {
	raw, ok, err := s.store.KVGet(ctx, catalogKVKey)
	if err != nil || !ok {
		return legacyCatalog
	}
	c, err := ParseCuratedCatalog([]byte(raw))
	if err != nil {
		return legacyCatalog
	}
	return c
}

// Init runs at startup: prices and suggestions come from the newest known
// catalogue and, when automatic sync is on, the embedded catalogue is applied.
func (s *CatalogSync) Init(ctx context.Context) {
	if s.cfg.GetBool("LLM_MODELS_AUTO_SYNC") {
		if _, err := s.Sync(ctx, false); err != nil {
			s.log.Warn("falha ao aplicar catálogo de modelos", "err", err)
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.applied(ctx)
	if b := BuiltinCatalog(); b.Revision > cur.Revision {
		cur = b
	}
	s.llm.SetCurated(cur)
	s.status = CatalogStatus{Revision: cur.Revision, Updated: cur.Updated, Source: "embutido"}
}

// Run is the scheduled job (CRON_MODELS).
func (s *CatalogSync) Run(ctx context.Context) error {
	if !s.cfg.GetBool("LLM_MODELS_AUTO_SYNC") {
		return nil
	}
	_, err := s.Sync(ctx, true)
	return err
}

// Status returns the last known state.
func (s *CatalogSync) Status() CatalogStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Sync applies the newest catalogue (embedded or, with remote, from GitHub).
func (s *CatalogSync) Sync(ctx context.Context, remote bool) (CatalogStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	applied := s.applied(ctx)
	best, source := BuiltinCatalog(), "embutido"
	var fetchErr error
	if remote {
		rc, err := s.fetch(ctx)
		switch {
		case err != nil:
			fetchErr = err
		case rc.Revision > best.Revision:
			best, source = rc, "GitHub"
		}
	}
	st := CatalogStatus{Revision: applied.Revision, Updated: applied.Updated, Source: "embutido", CheckedAt: time.Now()}
	if applied.Revision > BuiltinCatalog().Revision {
		st.Source = "GitHub"
	}
	if best.Revision <= applied.Revision {
		s.llm.SetCurated(applied)
	} else {
		plan := PlanCatalog(s.cfg.Get, applied, best)
		if len(plan.Changes) > 0 {
			if err := s.cfg.Update(plan.Changes); err != nil {
				return s.fail(st, err)
			}
		}
		raw, _ := json.Marshal(best)
		if err := s.store.KVSet(ctx, catalogKVKey, string(raw)); err != nil {
			return s.fail(st, err)
		}
		s.llm.SetCurated(best)
		s.llm.Reload(s.cfg)
		st.Revision, st.Updated, st.Source, st.Changes = best.Revision, best.Updated, source, plan.Summary()
		s.log.Info("catálogo de modelos atualizado", "revision", best.Revision, "source", source, "changes", strings.Join(st.Changes, "; "))
		if s.Notify != nil && len(st.Changes) > 0 {
			msg := fmt.Sprintf("🧩 Catálogo de modelos de IA atualizado (revisão %d):\n%s", best.Revision, strings.Join(st.Changes, "\n"))
			go func() {
				nctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_ = s.Notify(nctx, msg)
			}()
		}
	}
	if fetchErr != nil {
		st.Err = "não foi possível baixar o catálogo do GitHub: " + fetchErr.Error()
		s.log.Warn("catálogo de modelos remoto indisponível", "err", fetchErr)
	}
	s.status = st
	return st, fetchErr
}

func (s *CatalogSync) fail(st CatalogStatus, err error) (CatalogStatus, error) {
	st.Err = err.Error()
	s.status = st
	return st, err
}

func (s *CatalogSync) fetch(ctx context.Context) (*CuratedCatalog, error) {
	u := s.URL()
	if u == "" {
		return nil, errors.New("UPDATE_REPO inválido")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "second-brain")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, err
	}
	return ParseCuratedCatalog(b)
}
