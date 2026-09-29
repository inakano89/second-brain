package profile

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// AIWrite reports whether the chat may create and update profile items
// (PROFILE_AI_WRITE). It needs read access too: with PROFILE_AI_ACCESS=none the AI
// touches nothing.
func (s *Store) AIWrite() bool {
	return s.cfg.GetBool("PROFILE_AI_WRITE") && s.Access() != AccessNone
}

// weekdayAlias maps the spellings a person (or a model) may use to the stored keys.
var weekdayAlias = map[string]string{
	"dom": "dom", "domingo": "dom",
	"seg": "seg", "segunda": "seg",
	"ter": "ter", "terca": "ter", "terça": "ter",
	"qua": "qua", "quarta": "qua",
	"qui": "qui", "quinta": "qui",
	"sex": "sex", "sexta": "sex",
	"sab": "sab", "sáb": "sab", "sabado": "sab", "sábado": "sab",
}

// normWeekdays turns "segunda e quarta", "seg,qua" or "todos os dias" into the stored
// form ("seg,qua", week order; empty = every day).
func normWeekdays(v string) (string, error) {
	low := strings.ToLower(v)
	if strings.Contains(low, "todo") || strings.Contains(low, "diari") || strings.Contains(low, "diári") {
		return "", nil
	}
	picked := map[string]bool{}
	for _, w := range strings.FieldsFunc(low, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '/' }) {
		if w == "e" {
			continue
		}
		w = strings.TrimSuffix(w, "-feira")
		d, ok := weekdayAlias[w]
		if !ok {
			return "", fmt.Errorf("dia da semana inválido %q (use seg,ter,qua,qui,sex,sab,dom)", w)
		}
		picked[d] = true
	}
	var days []string
	for _, d := range Weekdays {
		if picked[d] {
			days = append(days, d)
		}
	}
	if len(days) == 7 {
		return "", nil
	}
	return strings.Join(days, ","), nil
}

// normalize validates the values against the kind and canonicalises weekdays and selects.
// Dates, times and numbers are checked by Save.
func (k *Kind) normalize(values map[string]string) (map[string]string, error) {
	fields := make(map[string]Field, len(k.Fields))
	keys := make([]string, 0, len(k.Fields))
	for _, f := range k.Fields {
		fields[f.Key] = f
		keys = append(keys, f.Key)
	}
	out := make(map[string]string, len(values))
	for key, v := range values {
		f, ok := fields[key]
		if !ok {
			return nil, fmt.Errorf("campo %q não existe em %q (campos: %s)", key, k.Key, strings.Join(keys, ", "))
		}
		v = strings.TrimSpace(v)
		switch {
		case v == "":
		case f.Type == FWeekdays:
			var err error
			if v, err = normWeekdays(v); err != nil {
				return nil, fmt.Errorf("%s: %w", f.Label, err)
			}
		case f.Type == FSelect:
			match := ""
			var opts []string
			for _, o := range f.Options {
				if o == "" {
					continue
				}
				opts = append(opts, o)
				if strings.EqualFold(o, v) {
					match = o
				}
			}
			if match == "" {
				return nil, fmt.Errorf("%s: use um destes valores: %s", f.Label, strings.Join(opts, ", "))
			}
			v = match
		}
		out[key] = v
	}
	return out, nil
}

// UpsertFromAI creates the item of the given kind and title, or updates the existing one
// with the same kind and title (case-insensitive): fields left out are kept, an empty
// value clears a field. Sensitivity is never changed by the AI: new items follow the
// kind's default and existing ones keep theirs. It returns the item and whether it was
// created. Callers must not echo the merged item back to a model; only what it sent.
func (s *Store) UpsertFromAI(ctx context.Context, kind, title string, values map[string]string) (Item, bool, error) {
	if !s.AIWrite() {
		return Item{}, false, errors.New("o usuário não liberou a gravação da IA no Perfil (PROFILE_AI_WRITE)")
	}
	k := KindOf(kind)
	if k == nil {
		keys := make([]string, len(Kinds))
		for i := range Kinds {
			keys[i] = Kinds[i].Key
		}
		return Item{}, false, fmt.Errorf("tipo desconhecido %q (tipos: %s)", kind, strings.Join(keys, ", "))
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return Item{}, false, fmt.Errorf("informe o título (%s)", k.TitleLabel)
	}
	vals, err := k.normalize(values)
	if err != nil {
		return Item{}, false, err
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()
	items, err := s.List(ctx, false)
	if err != nil {
		return Item{}, false, err
	}
	it := Item{Kind: k.Key, Title: title, Sensitive: k.Sensitive, Values: map[string]string{}}
	created := true
	for _, ex := range items {
		if ex.Kind == k.Key && !ex.Locked && strings.EqualFold(ex.Title, title) {
			it, created = ex, false
			if it.Values == nil {
				it.Values = map[string]string{}
			}
			break
		}
	}
	for key, v := range vals {
		if v == "" {
			delete(it.Values, key)
		} else {
			it.Values[key] = v
		}
	}
	if err := s.Save(ctx, &it); err != nil {
		return Item{}, false, err
	}
	s.log.Info("perfil gravado pela IA", "item", it.ID, "kind", it.Kind, "created", created, "fields", len(vals))
	return it, created, nil
}
