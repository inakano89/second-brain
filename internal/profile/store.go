package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
)

// AI access levels (PROFILE_AI_ACCESS).
const (
	AccessNone  = "none"  // the AI never reads the profile
	AccessBasic = "basic" // non-sensitive items always; sensitive ones only with a local model
	AccessFull  = "full"  // everything, cloud models included
)

// Item is a decrypted profile item.
type Item struct {
	ID        int64             `json:"id"`
	Kind      string            `json:"kind"`
	Title     string            `json:"title"`
	Values    map[string]string `json:"values,omitempty"`
	Sensitive bool              `json:"sensitive"`
	Archived  bool              `json:"archived"`
	Locked    bool              `json:"locked,omitempty"` // could not be decrypted
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Def returns the item's kind definition (never nil).
func (it Item) Def() *Kind {
	if k := KindOf(it.Kind); k != nil {
		return k
	}
	return &Kind{Key: it.Kind, Label: it.Kind, Icon: "•"}
}

// Get returns a trimmed value.
func (it Item) Get(k string) string { return strings.TrimSpace(it.Values[k]) }

// Date parses a YYYY-MM-DD value as a local day.
func (it Item) Date(k string, loc *time.Location) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02", it.Get(k), loc)
	return t, err == nil
}

// Num parses a numeric value (comma or dot decimals).
func (it Item) Num(k string) (float64, bool) {
	v := strings.ReplaceAll(it.Get(k), ",", ".")
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	return f, err == nil
}

// Times returns the normalised HH:MM list of a times field.
func (it Item) Times(k string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(it.Get(k), func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		if t, ok := normTime(p); ok {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// normTime accepts 8, 8h, 8:30, 08h30 and returns HH:MM.
func normTime(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Replace(s, "h", ":", 1)
	s = strings.TrimSuffix(s, ":")
	h, m, _ := strings.Cut(s, ":")
	hh, err := strconv.Atoi(h)
	if err != nil || hh < 0 || hh > 23 {
		return "", false
	}
	mm := 0
	if m != "" {
		if mm, err = strconv.Atoi(m); err != nil || mm < 0 || mm > 59 {
			return "", false
		}
	}
	return fmt.Sprintf("%02d:%02d", hh, mm), true
}

// OnWeekday reports whether the item happens on wd (no weekdays = every day).
func (it Item) OnWeekday(wd time.Weekday) bool {
	days := it.Get("weekdays")
	if days == "" {
		return true
	}
	return strings.Contains(","+days+",", ","+Weekdays[wd]+",")
}

// Store reads and writes profile items.
type Store struct {
	cfg *config.Config
	db  *database.DB
	log *slog.Logger
	v   *vault
}

// New creates the store.
func New(cfg *config.Config, db *database.DB, log *slog.Logger) *Store {
	return &Store{cfg: cfg, db: db, log: log.With("component", "profile"), v: &vault{cfg: cfg, db: db}}
}

// Location returns the configured timezone.
func (s *Store) Location() *time.Location { return s.cfg.Location() }

// Access returns the AI access level.
func (s *Store) Access() string {
	switch v := s.cfg.Get("PROFILE_AI_ACCESS"); v {
	case AccessNone, AccessFull:
		return v
	}
	return AccessBasic
}

func (s *Store) decode(ctx context.Context, r database.ProfileRow) Item {
	it := Item{ID: r.ID, Kind: r.Kind, Title: r.Title, Sensitive: r.Sensitive, Archived: r.Archived, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	title, err1 := s.v.decrypt(ctx, r.Title)
	data, err2 := s.v.decrypt(ctx, r.Data)
	if err1 != nil || err2 != nil {
		it.Title, it.Locked = "🔒 item protegido (chave ausente)", true
		return it
	}
	it.Title = title
	_ = json.Unmarshal([]byte(data), &it.Values)
	if it.Values == nil {
		it.Values = map[string]string{}
	}
	return it
}

// List returns the items (archived ones only when asked), ordered by kind catalogue and title.
func (s *Store) List(ctx context.Context, archived bool) ([]Item, error) {
	rows, err := s.db.ListProfile(ctx, archived)
	if err != nil {
		return nil, err
	}
	out := make([]Item, len(rows))
	for i, r := range rows {
		out[i] = s.decode(ctx, r)
	}
	order := map[string]int{}
	for i, k := range Kinds {
		order[k.Key] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return order[out[i].Kind] < order[out[j].Kind]
		}
		return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title)
	})
	return out, nil
}

// Get returns one item.
func (s *Store) Get(ctx context.Context, id int64) (Item, error) {
	r, err := s.db.GetProfile(ctx, id)
	if err != nil {
		return Item{}, err
	}
	it := s.decode(ctx, r)
	if it.Locked {
		return it, ErrLocked
	}
	return it, nil
}

// Save validates, encrypts (when sensitive) and stores the item, setting its ID.
func (s *Store) Save(ctx context.Context, it *Item) error {
	k := KindOf(it.Kind)
	if k == nil {
		return fmt.Errorf("tipo desconhecido: %q", it.Kind)
	}
	it.Title = strings.TrimSpace(it.Title)
	if it.Title == "" {
		return fmt.Errorf("preencha %q", k.TitleLabel)
	}
	vals := map[string]string{}
	for _, f := range k.Fields {
		v := strings.TrimSpace(it.Values[f.Key])
		if v == "" {
			continue
		}
		switch f.Type {
		case FDate:
			if _, err := time.Parse("2006-01-02", v); err != nil {
				return fmt.Errorf("%s: data inválida", f.Label)
			}
		case FTime:
			t, ok := normTime(v)
			if !ok {
				return fmt.Errorf("%s: horário inválido", f.Label)
			}
			v = t
		case FTimes:
			tmp := Item{Values: map[string]string{"t": v}}
			ts := tmp.Times("t")
			if len(ts) == 0 {
				return fmt.Errorf("%s: use horários como 08:00, 20:00", f.Label)
			}
			v = strings.Join(ts, ", ")
		case FNumber, FDay:
			n, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", "."), 64)
			if err != nil || (f.Type == FDay && (n < 1 || n > 31)) {
				return fmt.Errorf("%s: número inválido", f.Label)
			}
		}
		vals[f.Key] = v
	}
	it.Values = vals
	data, _ := json.Marshal(vals)
	row := database.ProfileRow{ID: it.ID, Kind: it.Kind, Title: it.Title, Data: string(data), Sensitive: it.Sensitive, Archived: it.Archived}
	if it.Sensitive {
		var err error
		if row.Title, err = s.v.encrypt(ctx, it.Title); err != nil {
			return err
		}
		if row.Data, err = s.v.encrypt(ctx, string(data)); err != nil {
			return err
		}
	}
	id, err := s.db.SaveProfile(ctx, row)
	if err != nil {
		return err
	}
	it.ID = id
	return nil
}

// Delete removes an item.
func (s *Store) Delete(ctx context.Context, id int64) error { return s.db.DeleteProfile(ctx, id) }

// DayKey formats a local day.
func DayKey(t time.Time) string { return t.Format("2006-01-02") }

func checkKey(id int64, day, slot string) string { return fmt.Sprintf("%d|%s|%s", id, day, slot) }

// Checks returns the check-ins between two days as a set.
func (s *Store) Checks(ctx context.Context, from, to time.Time) (map[string]bool, error) {
	list, err := s.db.ProfileChecks(ctx, DayKey(from), DayKey(to))
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list))
	for _, c := range list {
		out[checkKey(c.ItemID, c.Day, c.Slot)] = true
	}
	return out, nil
}

// Check marks a slot as done (or undoes it) and adjusts the stock of medications and
// supplements. It reports whether anything changed.
func (s *Store) Check(ctx context.Context, id int64, day time.Time, slot string, undo bool) (Item, bool, error) {
	it, err := s.Get(ctx, id)
	if err != nil {
		return it, false, err
	}
	var changed bool
	if undo {
		changed, err = s.db.UncheckProfile(ctx, id, DayKey(day), slot)
	} else {
		changed, err = s.db.CheckProfile(ctx, id, DayKey(day), slot)
	}
	if err != nil || !changed {
		return it, false, err
	}
	if stock, ok := it.Num("stock"); ok && (it.Kind == "medication" || it.Kind == "supplement") {
		per, ok := it.Num("per_dose")
		if !ok || per <= 0 {
			per = 1
		}
		if undo {
			stock += per
		} else {
			stock = max(0, stock-per)
		}
		it.Values["stock"] = strconv.FormatFloat(stock, 'f', -1, 64)
		if err := s.Save(ctx, &it); err != nil {
			return it, true, err
		}
	}
	return it, true, nil
}

// CheckNow marks the slot of item id closest to now (the last one already due, or the
// next one within an hour); items without times use the empty slot.
func (s *Store) CheckNow(ctx context.Context, id int64, now time.Time) (Item, string, bool, error) {
	it, err := s.Get(ctx, id)
	if err != nil {
		return it, "", false, err
	}
	slot := ""
	times := it.Times("times")
	if len(times) == 0 {
		if t := it.Get("time"); t != "" {
			times = []string{t}
		}
	}
	if len(times) > 0 {
		done, _ := s.Checks(ctx, now, now)
		limit := now.Add(time.Hour).Format("15:04")
		slot = times[0]
		for _, t := range times {
			if t <= limit && !done[checkKey(id, DayKey(now), t)] {
				slot = t
			}
		}
	}
	it, changed, err := s.Check(ctx, id, now, slot, false)
	return it, slot, changed, err
}

// ForAI returns the items the AI may read under the current policy. local reports
// whether the model that will read them runs locally.
func (s *Store) ForAI(ctx context.Context, query, kind string, local bool) (map[string]any, error) {
	access := s.Access()
	if access == AccessNone {
		return map[string]any{"error": "o usuário desativou o acesso da IA ao perfil (PROFILE_AI_ACCESS=none)"}, nil
	}
	items, err := s.List(ctx, false)
	if err != nil {
		return nil, err
	}
	allowSensitive := access == AccessFull || local
	var list []map[string]any
	hidden := 0
	for _, it := range items {
		if kind != "" && it.Kind != kind {
			continue
		}
		if it.Locked {
			continue
		}
		if it.Sensitive && !allowSensitive {
			hidden++
			continue
		}
		m := map[string]any{"id": it.ID, "kind": it.Def().Label, "title": it.Title}
		for _, f := range it.Def().Fields {
			if v := it.Get(f.Key); v != "" {
				m[f.Label] = v
			}
		}
		list = append(list, m)
	}
	if q := strings.ToLower(strings.TrimSpace(query)); q != "" {
		var hits []map[string]any
		for _, m := range list {
			b, _ := json.Marshal(m)
			text := strings.ToLower(string(b))
			for _, w := range strings.Fields(q) {
				if len(w) > 2 && strings.Contains(text, w) {
					hits = append(hits, m)
					break
				}
			}
		}
		if len(hits) > 0 {
			list = hits
		}
	}
	out := map[string]any{"items": list, "today": time.Now().In(s.Location()).Format("2006-01-02 (Monday)")}
	if hidden > 0 {
		out["hidden_sensitive"] = fmt.Sprintf("%d itens sensíveis (saúde, documentos, endereços) não foram enviados por privacidade. Para usá-los, o usuário pode usar um modelo local ou mudar PROFILE_AI_ACCESS.", hidden)
	}
	s.log.Info("perfil consultado pela IA", "items", len(list), "hidden", hidden, "local", local)
	return out, nil
}

// People returns person nodes that have a birthday, for the upcoming list.
func (s *Store) People(ctx context.Context) ([]Person, error) {
	nodes, err := s.db.ListNodes(ctx, database.NodeFilter{Types: []string{database.TypePerson}, Limit: 5000})
	if err != nil {
		return nil, err
	}
	var out []Person
	for _, n := range nodes {
		if b, ok := n.Meta["birthday"].(string); ok && b != "" {
			out = append(out, Person{ID: n.ID, Name: n.Title, Birthday: b})
		}
	}
	return out, nil
}

// Imported returns the calendar event IDs already added to the profile.
func (s *Store) Imported(ctx context.Context) map[string]bool {
	var ids []string
	_, _ = s.db.KVGetJSON(ctx, "profile.imported", &ids)
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// MarkImported remembers that a calendar event became a profile item.
func (s *Store) MarkImported(ctx context.Context, eventID string) error {
	if eventID == "" {
		return errors.New("evento sem id")
	}
	var ids []string
	_, _ = s.db.KVGetJSON(ctx, "profile.imported", &ids)
	for _, id := range ids {
		if id == eventID {
			return nil
		}
	}
	ids = append(ids, eventID)
	if len(ids) > 2000 {
		ids = ids[len(ids)-2000:]
	}
	return s.db.KVSetJSON(ctx, "profile.imported", ids)
}
