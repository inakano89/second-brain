package takeout

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Recognised formats.
const (
	fmtActivity = "activity"          // My Activity (YouTube, Search, Maps, Chrome, Play, Gemini…)
	fmtChrome   = "chrome"            // Chrome browser history
	fmtSemantic = "semantic-location" // old Takeout Semantic Location History
	fmtTimeline = "timeline"          // Timeline export from the Maps app (Android/iOS)
	fmtPlaces   = "places"            // GeoJSON saved places / reviews
	fmtPlay     = "play"              // Google Play Store
)

// item is one timestamped entry of a monthly stream.
type item struct {
	t     time.Time
	text  string
	url   string
	extra string // channel, domain, address…
	key   string // what "most frequent" counts
}

type stream struct {
	kind, label string
	tags        []string
	items       []item
}

type listNote struct {
	ref, title string
	tags       []string
	lines      []string
}

// batch accumulates what one file yields before merging into the collection.
type batch struct {
	loc     *time.Location
	streams map[string]*stream
	lists   map[string]*listNote
	n       int
}

func newBatch(loc *time.Location) *batch {
	return &batch{loc: loc, streams: map[string]*stream{}, lists: map[string]*listNote{}}
}

func (b *batch) add(kind, label string, tags []string, it item) {
	if it.t.IsZero() || strings.TrimSpace(it.text) == "" {
		return
	}
	st := b.streams[kind]
	if st == nil {
		st = &stream{kind: kind, label: label, tags: tags}
		b.streams[kind] = st
	}
	st.items = append(st.items, it)
	b.n++
}

func (b *batch) list(ref, title string, tags []string, line string) {
	l := b.lists[ref]
	if l == nil {
		l = &listNote{ref: ref, title: title, tags: tags}
		b.lists[ref] = l
	}
	l.lines = append(l.lines, line)
	b.n++
}

// collection gathers every file of an archive (safe for concurrent parsers).
type collection struct {
	loc      *time.Location
	mu       sync.Mutex
	streams  map[string]*stream
	lists    map[string]*listNote
	formats  map[string]int
	items    int
	warnings []string
	html     bool
}

func newCollection(loc *time.Location) *collection {
	return &collection{loc: loc, streams: map[string]*stream{}, lists: map[string]*listNote{}, formats: map[string]int{}}
}

func (c *collection) merge(format string, b *batch) {
	if format == "" || b.n == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.formats[format]++
	c.items += b.n
	for k, st := range b.streams {
		if cur := c.streams[k]; cur != nil {
			cur.items = append(cur.items, st.items...)
		} else {
			c.streams[k] = st
		}
	}
	for k, l := range b.lists {
		if cur := c.lists[k]; cur != nil {
			cur.lines = append(cur.lines, l.lines...)
		} else {
			c.lists[k] = l
		}
	}
}

func (c *collection) warn(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.warnings) < 20 {
		c.warnings = append(c.warnings, msg)
	}
}

func (c *collection) warnHTML(name string) {
	c.mu.Lock()
	first := !c.html
	c.html = true
	c.mu.Unlock()
	if first {
		c.warn("histórico em HTML (" + path.Base(name) + "): no Takeout, em “Vários formatos”, escolha JSON para importá-lo")
	}
}

// ActivityHTML spots My Activity / YouTube history exported as HTML (only JSON is importable).
func ActivityHTML(name string) bool {
	b := strings.ToLower(path.Base(name))
	for _, k := range []string{"activity", "atividade", "actividad", "history", "histórico", "historico", "historial"} {
		if strings.Contains(b, k) {
			return true
		}
	}
	return false
}

func (c *collection) parse(name string, r io.Reader) error {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<16))
	tok, err := dec.Token()
	if err != nil {
		return nil // empty or not JSON
	}
	switch tok {
	case json.Delim('['):
		return c.parseArray(name, dec)
	case json.Delim('{'):
		return c.parseObject(name, dec)
	}
	return nil
}

// parseArray streams a top-level array, detecting the format from its first element.
func (c *collection) parseArray(name string, dec *json.Decoder) error {
	b := newBatch(c.loc)
	format := ""
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			c.merge(format, b)
			return err
		}
		if format == "" {
			if format = detectElement(raw); format == "" {
				return nil
			}
		}
		switch format {
		case fmtActivity:
			b.activity(raw)
		case fmtPlay:
			b.play(raw)
		case fmtTimeline:
			b.segment(raw)
		}
	}
	c.merge(format, b)
	return nil
}

func detectElement(raw json.RawMessage) string {
	var p map[string]json.RawMessage
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	has := func(k string) bool { _, ok := p[k]; return ok }
	switch {
	case has("header") && has("time") && has("title"):
		return fmtActivity
	case has("startTime") && (has("visit") || has("activity") || has("timelinePath")):
		return fmtTimeline
	case len(p) == 1:
		for k := range p {
			if _, ok := playKinds[k]; ok {
				return fmtPlay
			}
		}
	}
	return ""
}

// parseObject walks a top-level object, streaming the big arrays it knows.
func (c *collection) parseObject(name string, dec *json.Decoder) error {
	b := newBatch(c.loc)
	format := ""
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := t.(string)
		switch key {
		case "Browser History":
			format, err = fmtChrome, eachElement(dec, b.chrome)
		case "timelineObjects":
			format, err = fmtSemantic, eachElement(dec, b.semantic)
		case "semanticSegments":
			format, err = fmtTimeline, eachElement(dec, b.segment)
		case "features":
			format, err = fmtPlaces, eachElement(dec, func(raw json.RawMessage) { b.feature(name, raw) })
		default:
			err = skipValue(dec)
		}
		if err != nil {
			c.merge(format, b)
			return err
		}
	}
	c.merge(format, b)
	return nil
}

// eachElement streams the array value at the decoder's position.
func eachElement(dec *json.Decoder, fn func(json.RawMessage)) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('[') {
		return errors.New("lista esperada")
	}
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		fn(raw)
	}
	_, err = dec.Token()
	return err
}

// skipValue consumes one value without materialising it.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		switch t {
		case json.Delim('['), json.Delim('{'):
			depth++
		case json.Delim(']'), json.Delim('}'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// slug makes a lowercase ASCII-ish identifier for refs and tags.
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 && !strings.ContainsRune("—–·°", r) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// topKeys returns "a (3), b (2)" for the n most frequent keys.
func topKeys(items []item, n int) string {
	counts := map[string]int{}
	for _, it := range items {
		if k := strings.TrimSpace(it.key); k != "" {
			counts[k]++
		}
	}
	type kv struct {
		k string
		v int
	}
	var kvs []kv
	for k, v := range counts {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].v == kvs[j].v {
			return kvs[i].k < kvs[j].k
		}
		return kvs[i].v > kvs[j].v
	})
	var parts []string
	for i := 0; i < len(kvs) && i < n; i++ {
		parts = append(parts, fmt.Sprintf("%s (%d)", kvs[i].k, kvs[i].v))
	}
	return strings.Join(parts, ", ")
}
