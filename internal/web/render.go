package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/extract"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/mdhtml"
	"github.com/inakano89/second-brain/internal/profile"
)

var weekdayNames = []string{"Domingo", "Segunda", "Terça", "Quarta", "Quinta", "Sexta", "Sábado"}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(s.Cfg.Location()).Format("02/01/2006 15:04")
		},
		"fmtDate": func(t *time.Time) string {
			if t == nil || t.IsZero() {
				return ""
			}
			return t.In(s.Cfg.Location()).Format("02/01/2006 15:04")
		},
		"inputDate": func(t *time.Time) string {
			if t == nil || t.IsZero() {
				return ""
			}
			return t.In(s.Cfg.Location()).Format("2006-01-02T15:04")
		},
		"since": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			d := time.Since(t)
			switch {
			case d < 0:
				return "em " + humanDur(-d)
			case d < time.Minute:
				return "agora"
			}
			return "há " + humanDur(d)
		},
		"until":     func(t time.Time) string { return humanDur(time.Until(t)) },
		"truncate":  extract.Truncate,
		"join":      strings.Join,
		"typeLabel": func(t string) string { return typeLabels[t] },
		"typeColor": func(t string) template.CSS { return template.CSS(typeColors[t]) }, // constant var(--t-*) values
		"markdown":  Markdown,
		"json": func(v any) string {
			b, _ := json.Marshal(v)
			return string(b)
		},
		"usd":      func(v float64) string { return fmt.Sprintf("US$ %.4f", v) },
		"weekdays": func() []string { return profile.Weekdays },
		"kindLabel": func(k string) string {
			if d := profile.KindOf(k); d != nil {
				return d.Label
			}
			return k
		},
		"fmtDay": func(t time.Time) string {
			return weekdayNames[t.Weekday()] + ", " + t.Format("02/01/2006")
		},
		"rfc3339": func(t time.Time) string { return t.Format(time.RFC3339) },
		"dict": func(kv ...any) map[string]any {
			m := make(map[string]any, len(kv)/2)
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
		"num":       func(v int64) string { return humanInt(v) },
		"pct":       func(v float64) string { return fmt.Sprintf("%.0f%%", v) },
		"safeURL":   func(s string) template.URL { return template.URL(s) },
		"urlquery":  url.QueryEscape,
		"hasPrefix": strings.HasPrefix,
		"split":     func(s string) []string { return strings.Split(s, ",") },
		"routeKey":  llm.RouteKey,
		"price": func(p *llm.Price) string {
			if p == nil {
				return "preço desconhecido"
			}
			return fmt.Sprintf("US$ %g / %g", p.In, p.Out)
		},
		"helpAnchor": func(group string) string { return helpAnchors[group] },
		"metricVal": func(kind string, v float64) string {
			if strings.HasSuffix(kind, "_minutes") {
				return fmt.Sprintf("%dh%02d", int(v)/60, int(v)%60)
			}
			return fmt.Sprintf("%.0f", v)
		},
		"levelClass":   func(l string) string { return "lvl-" + strings.ToLower(l) },
		"importState":  func(st string) string { return importStates[st] },
		"sourceLabel":  sourceLabel,
		"nodeDate":     func(n database.Node) string { return nodeDate(n, s.Cfg.Location()) },
		"importedNote": func(n database.Node) string { return importedNote(n, s.Cfg.Location()) },
		"originLabel":  func(o string) string { return originLabels[o] },
		"brl": func(v float64) string {
			neg := v < 0
			if neg {
				v = -v
			}
			s := fmt.Sprintf("%.2f", v)
			whole, cents, _ := strings.Cut(s, ".")
			var parts []string
			for len(whole) > 3 {
				parts = append([]string{whole[len(whole)-3:]}, parts...)
				whole = whole[:len(whole)-3]
			}
			parts = append([]string{whole}, parts...)
			out := "R$ " + strings.Join(parts, ".") + "," + cents
			if neg {
				out = "−" + out
			}
			return out
		},
		"isUnknownDate": func(n database.Node) bool { return n.DateUnknown() },
	}
}

func humanDur(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d dias", int(d.Hours()/24))
}

func humanInt(v int64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(v)/1e6)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", float64(v)/1e3)
	}
	return fmt.Sprint(v)
}

// Markdown renders a safe subset of Markdown to HTML (all input is escaped first). [[Links]] search
// the graph.
func Markdown(src string) template.HTML {
	return template.HTML(mdhtml.Render(src, func(target, label string) string {
		return `<a class="wikilink" href="/?q=` + url.QueryEscape(target) + `">` + label + `</a>`
	}))
}
