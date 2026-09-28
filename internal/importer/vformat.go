package importer

import (
	"fmt"
	"io"
	"mime/quotedprintable"
	"strings"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/inakano89/second-brain/internal/database"
)

// Shared reader for the RFC 6350 (vCard) and RFC 5545 (iCalendar) line formats.

type prop struct {
	name   string
	params map[string]string
	value  string
}

// unfold joins folded lines (leading space/tab) and quoted-printable soft breaks (vCard 2.1).
func unfold(doc string) []string {
	doc = strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(doc, "\ufeff"), "\r\n", "\n"), "\r", "\n")
	var lines []string
	qpCont := false
	for _, l := range strings.Split(doc, "\n") {
		n := len(lines)
		switch {
		case n > 0 && qpCont:
			lines[n-1] = strings.TrimSuffix(lines[n-1], "=") + l
		case n > 0 && (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")):
			lines[n-1] += l[1:]
		default:
			lines = append(lines, l)
		}
		last := lines[len(lines)-1]
		head, _, _ := strings.Cut(last, ":")
		qpCont = strings.Contains(strings.ToUpper(head), "QUOTED-PRINTABLE") && strings.HasSuffix(last, "=")
	}
	return lines
}

func parseProp(line string) (prop, bool) {
	idx, inQ := -1, false
	for i, r := range line {
		if r == '"' {
			inQ = !inQ
		}
		if r == ':' && !inQ {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return prop{}, false
	}
	parts := strings.Split(line[:idx], ";")
	name := strings.ToUpper(strings.TrimSpace(parts[0]))
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:] // item1.EMAIL → EMAIL
	}
	pr := prop{name: name, params: map[string]string{}, value: line[idx+1:]}
	for _, pp := range parts[1:] {
		k, v, ok := strings.Cut(pp, "=")
		if !ok {
			k, v = "TYPE", pp // vCard 2.1: TEL;CELL:...
		}
		k = strings.ToUpper(strings.TrimSpace(k))
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if pr.params[k] != "" {
			v = pr.params[k] + "," + v
		}
		pr.params[k] = v
	}
	if strings.EqualFold(pr.params["ENCODING"], "QUOTED-PRINTABLE") {
		if b, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(pr.value))); err == nil {
			pr.value = string(b)
		}
	}
	if cs := pr.params["CHARSET"]; cs != "" && !strings.EqualFold(cs, "utf-8") {
		if r, err := charset.NewReaderLabel(cs, strings.NewReader(pr.value)); err == nil {
			if b, err := io.ReadAll(r); err == nil {
				pr.value = string(b)
			}
		}
	}
	return pr, true
}

var textUnescaper = strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`)

func unescapeText(s string) string { return strings.TrimSpace(textUnescaper.Replace(s)) }

// splitStructured splits on unescaped sep (N, ADR, ORG components).
func splitStructured(s string, sep byte) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			cur.WriteByte(s[i])
			cur.WriteByte(s[i+1])
			i++
		case s[i] == sep:
			out = append(out, unescapeText(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	return append(out, unescapeText(cur.String()))
}

func joinNonEmpty(parts []string, sep string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// components groups properties of each BEGIN:<kind> … END:<kind> block (nested blocks ignored).
func components(lines []string, kinds ...string) (blocks [][]prop, kindsOut []string, root []prop) {
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var stack []string
	var cur []prop
	for _, l := range lines {
		u := strings.ToUpper(strings.TrimSpace(l))
		switch {
		case strings.HasPrefix(u, "BEGIN:"):
			stack = append(stack, u[6:])
			if want[u[6:]] {
				cur = nil
			}
			continue
		case strings.HasPrefix(u, "END:"):
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if want[top] {
					blocks = append(blocks, cur)
					kindsOut = append(kindsOut, top)
				}
			}
			continue
		}
		if len(stack) == 0 {
			continue
		}
		pr, ok := parseProp(l)
		if !ok {
			continue
		}
		if want[stack[len(stack)-1]] {
			cur = append(cur, pr)
		} else if len(stack) == 1 {
			root = append(root, pr)
		}
	}
	return
}

// ---- vCard ----

var ignoredCategories = map[string]bool{"mycontacts": true, "* mycontacts": true, "starred": true, "* starred": true}

func (p *parser) parseVCard(doc string) []Item {
	cards, _, _ := components(unfold(doc), "VCARD")
	var items []Item
	for _, card := range cards {
		var fn, uid, nick, title, bday, note string
		var nameParts, emails, phones, orgs, addrs, urls, tags []string
		for _, pr := range card {
			v := pr.value
			switch pr.name {
			case "FN":
				fn = unescapeText(v)
			case "N":
				c := splitStructured(v, ';')
				for len(c) < 5 {
					c = append(c, "")
				}
				nameParts = []string{c[3], c[1], c[2], c[0], c[4]}
			case "NICKNAME":
				nick = unescapeText(v)
			case "EMAIL":
				if e := unescapeText(v); e != "" {
					emails = append(emails, e)
				}
			case "TEL":
				if t := unescapeText(strings.TrimPrefix(v, "tel:")); t != "" {
					if typ := strings.ToLower(pr.params["TYPE"]); typ != "" {
						t += " (" + strings.ReplaceAll(typ, ",", ", ") + ")"
					}
					phones = append(phones, t)
				}
			case "ORG":
				if o := joinNonEmpty(splitStructured(v, ';'), " / "); o != "" {
					orgs = append(orgs, o)
				}
			case "TITLE", "ROLE":
				title = joinNonEmpty([]string{title, unescapeText(v)}, ", ")
			case "BDAY":
				bday = strings.TrimSpace(v)
			case "NOTE":
				note = unescapeText(v)
			case "ADR":
				if a := joinNonEmpty(splitStructured(v, ';'), ", "); a != "" {
					addrs = append(addrs, a)
				}
			case "URL", "X-SOCIALPROFILE":
				if u := unescapeText(v); u != "" {
					urls = append(urls, u)
				}
			case "CATEGORIES":
				for _, c := range splitStructured(v, ',') {
					if c != "" && !ignoredCategories[strings.ToLower(c)] {
						tags = append(tags, c)
					}
				}
			case "UID":
				uid = strings.TrimSpace(v)
			}
		}
		name := firstNonEmpty(fn, joinNonEmpty(nameParts, " "), nick, strings.Join(emails, ""))
		if name == "" {
			continue
		}
		var b strings.Builder
		line := func(label string, vals ...string) {
			if s := joinNonEmpty(vals, ", "); s != "" {
				fmt.Fprintf(&b, "**%s:** %s\n", label, s)
			}
		}
		line("Apelido", nick)
		line("E-mail", emails...)
		line("Telefone", phones...)
		line("Organização", orgs...)
		line("Cargo", title)
		meta := map[string]any{}
		if bday != "" {
			if d, ok := parseDate(strings.TrimPrefix(bday, "--"), p.loc); ok && !strings.HasPrefix(bday, "--") {
				line("Aniversário", d.Format("02/01/2006"))
				meta["birthday"] = d.Format("2006-01-02")
			} else {
				line("Aniversário", bday)
				meta["birthday"] = bday
			}
		}
		line("Endereço", addrs...)
		line("Site", urls...)
		if note != "" {
			b.WriteString("\n" + note + "\n")
		}
		if len(emails) > 0 {
			meta["emails"] = emails
		}
		if len(phones) > 0 {
			meta["phones"] = phones
		}
		if len(orgs) > 0 {
			meta["org"] = orgs[0]
		}
		ref := uid
		if ref == "" {
			ref = hashRef(name, strings.Join(emails, ","), strings.Join(phones, ","))
		}
		items = append(items, Item{
			Format: FormatVCard, Ref: ref, Type: database.TypePerson, Title: name, Content: strings.TrimSpace(b.String()),
			Tags: append(tags, "contato"), Meta: meta,
		})
	}
	return items
}

// ---- iCalendar ----

func (p *parser) icalTime(pr prop) (t time.Time, allDay, ok bool) {
	v := strings.TrimSpace(pr.value)
	if v == "" {
		return
	}
	if strings.EqualFold(pr.params["VALUE"], "DATE") || len(v) == 8 {
		t, err := time.ParseInLocation("20060102", v, p.loc)
		return t, true, err == nil
	}
	if strings.HasSuffix(v, "Z") {
		t, err := time.Parse("20060102T150405Z", v)
		return t, false, err == nil
	}
	loc := p.loc
	if tz := strings.Trim(pr.params["TZID"], "/ "); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	t, err := time.ParseInLocation("20060102T150405", v, loc)
	return t, false, err == nil
}

func (p *parser) parseICal(doc string) []Item {
	blocks, kinds, root := components(unfold(doc), "VEVENT", "VTODO")
	calName := ""
	for _, pr := range root {
		if pr.name == "X-WR-CALNAME" {
			calName = unescapeText(pr.value)
		}
	}
	var items []Item
	for i, props := range blocks {
		get := map[string]prop{}
		var people, cats []string
		for _, pr := range props {
			switch pr.name {
			case "ATTENDEE", "ORGANIZER":
				if cn := strings.TrimSpace(pr.params["CN"]); cn != "" && !strings.Contains(cn, "@") {
					people = append(people, "[["+cn+"]]")
				} else if m := strings.TrimPrefix(strings.TrimPrefix(pr.value, "mailto:"), "MAILTO:"); m != "" {
					people = append(people, m)
				}
			case "CATEGORIES":
				cats = append(cats, splitStructured(pr.value, ',')...)
			default:
				if _, ok := get[pr.name]; !ok {
					get[pr.name] = pr
				}
			}
		}
		if strings.EqualFold(get["STATUS"].value, "CANCELLED") {
			continue
		}
		title := firstNonEmpty(unescapeText(get["SUMMARY"].value), "(sem título)")
		desc := unescapeText(get["DESCRIPTION"].value)
		uid := strings.TrimSpace(get["UID"].value)
		it := Item{Format: FormatICal, Title: title, Tags: cats, Meta: map[string]any{}, URL: strings.TrimSpace(get["URL"].value)}
		if calName != "" {
			it.Meta["calendar"] = calName
		}
		if c, _, ok := p.icalTime(get["CREATED"]); ok {
			it.CreatedAt = c
		}
		if kinds[i] == "VTODO" {
			it.Type, it.Status, it.Content = database.TypeTask, database.StatusOpen, desc
			if strings.EqualFold(get["STATUS"].value, "COMPLETED") || get["COMPLETED"].value != "" {
				it.Status = database.StatusDone
			}
			if d, _, ok := p.icalTime(get["DUE"]); ok {
				it.DueAt = &d
			}
			it.Ref = firstNonEmpty(uid, hashRef("todo", title, get["DUE"].value))
			items = append(items, it)
			continue
		}
		start, allDay, ok := p.icalTime(get["DTSTART"])
		if !ok {
			continue
		}
		end, _, ok := p.icalTime(get["DTEND"])
		if !ok || end.Before(start) {
			end = start.Add(time.Hour)
			if allDay {
				end = start.AddDate(0, 0, 1)
			}
		}
		layout := "02/01/2006 15:04"
		if allDay {
			layout = "02/01/2006"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "**Início:** %s\n**Fim:** %s\n", start.In(p.loc).Format(layout), end.In(p.loc).Format(layout))
		if loc := unescapeText(get["LOCATION"].value); loc != "" {
			fmt.Fprintf(&b, "**Local:** %s\n", loc)
			it.Meta["location"] = loc
		}
		if len(people) > 0 {
			fmt.Fprintf(&b, "**Participantes:** %s\n", strings.Join(people, ", "))
		}
		if rr := strings.TrimSpace(get["RRULE"].value); rr != "" {
			fmt.Fprintf(&b, "**Recorrência:** %s\n", rr)
			it.Meta["rrule"] = rr
		}
		if desc != "" {
			b.WriteString("\n" + desc + "\n")
		}
		it.Type, it.Content, it.DueAt = database.TypeEvent, strings.TrimSpace(b.String()), &start
		it.Meta["start"], it.Meta["end"], it.Meta["all_day"] = start.Format(time.RFC3339), end.Format(time.RFC3339), allDay
		it.Ref = firstNonEmpty(uid, hashRef(title, get["DTSTART"].value))
		if rid := strings.TrimSpace(get["RECURRENCE-ID"].value); rid != "" {
			it.Ref += "#" + rid
		}
		it.Tags = append(it.Tags, "agenda")
		items = append(items, it)
	}
	return items
}
