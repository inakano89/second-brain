package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/database"
)

const (
	peopleAPI    = "https://people.googleapis.com/v1/"
	personFields = "names,nicknames,emailAddresses,phoneNumbers,organizations,birthdays,addresses,biographies,urls,relations,events"
	searchMask   = "names,emailAddresses,phoneNumbers,organizations"
)

type gDate struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	Day   int `json:"day"`
}

func (d *gDate) String() string {
	if d == nil || d.Month == 0 || d.Day == 0 {
		return ""
	}
	if d.Year > 0 {
		return fmt.Sprintf("%02d/%02d/%04d", d.Day, d.Month, d.Year)
	}
	return fmt.Sprintf("%02d/%02d", d.Day, d.Month)
}

type gValue struct {
	Value         string `json:"value"`
	FormattedType string `json:"formattedType"`
}

type gPerson struct {
	ResourceName string `json:"resourceName"`
	Names        []struct {
		DisplayName string `json:"displayName"`
	} `json:"names"`
	Nicknames      []gValue `json:"nicknames"`
	EmailAddresses []gValue `json:"emailAddresses"`
	PhoneNumbers   []struct {
		Value         string `json:"value"`
		CanonicalForm string `json:"canonicalForm"`
		FormattedType string `json:"formattedType"`
	} `json:"phoneNumbers"`
	Organizations []struct {
		Name       string `json:"name"`
		Title      string `json:"title"`
		Department string `json:"department"`
	} `json:"organizations"`
	Birthdays []struct {
		Date *gDate `json:"date"`
		Text string `json:"text"`
	} `json:"birthdays"`
	Addresses []struct {
		FormattedValue string `json:"formattedValue"`
		FormattedType  string `json:"formattedType"`
	} `json:"addresses"`
	Biographies []gValue `json:"biographies"`
	Urls        []gValue `json:"urls"`
	Relations   []struct {
		Person        string `json:"person"`
		FormattedType string `json:"formattedType"`
	} `json:"relations"`
	Events []struct {
		Date          *gDate `json:"date"`
		FormattedType string `json:"formattedType"`
	} `json:"events"`
}

func (p *gPerson) emails() []string {
	var out []string
	for _, e := range p.EmailAddresses {
		if v := strings.ToLower(strings.TrimSpace(e.Value)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (p *gPerson) phones() []string {
	var out []string
	for _, ph := range p.PhoneNumbers {
		v := ph.CanonicalForm
		if v == "" {
			v = ph.Value
		}
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (p *gPerson) name() string {
	for _, n := range p.Names {
		if s := strings.TrimSpace(n.DisplayName); s != "" {
			return s
		}
	}
	if e := p.emails(); len(e) > 0 {
		return e[0]
	}
	return ""
}

func (p *gPerson) contact(other bool) agent.Contact {
	c := agent.Contact{Resource: p.ResourceName, Name: p.name(), Emails: p.emails(), Phones: p.phones(), Other: other}
	if len(p.Organizations) > 0 {
		c.Company, c.Title = p.Organizations[0].Name, p.Organizations[0].Title
	}
	return c
}

func typed(v, t string) string {
	if t == "" {
		return v
	}
	return v + " (" + t + ")"
}

// markdown renders every known field of the contact.
func (p *gPerson) markdown() string {
	var b strings.Builder
	line := func(label string, vals []string) {
		if len(vals) > 0 {
			fmt.Fprintf(&b, "**%s:** %s\n", label, strings.Join(vals, ", "))
		}
	}
	line("E-mails", p.emails())
	var phones []string
	for _, ph := range p.PhoneNumbers {
		phones = append(phones, typed(ph.Value, ph.FormattedType))
	}
	line("Telefones", phones)
	var orgs []string
	for _, o := range p.Organizations {
		s := strings.Join(nonEmpty(o.Name, o.Title, o.Department), " — ")
		if s != "" {
			orgs = append(orgs, s)
		}
	}
	line("Trabalho", orgs)
	var nicks []string
	for _, n := range p.Nicknames {
		nicks = append(nicks, n.Value)
	}
	line("Apelidos", nicks)
	for _, bd := range p.Birthdays {
		if s := bd.Date.String(); s != "" {
			fmt.Fprintf(&b, "**Aniversário:** %s\n", s)
			break
		} else if bd.Text != "" {
			fmt.Fprintf(&b, "**Aniversário:** %s\n", bd.Text)
			break
		}
	}
	var events []string
	for _, e := range p.Events {
		if s := e.Date.String(); s != "" {
			events = append(events, typed(s, e.FormattedType))
		}
	}
	line("Datas", events)
	var addrs []string
	for _, a := range p.Addresses {
		if a.FormattedValue != "" {
			addrs = append(addrs, typed(strings.ReplaceAll(a.FormattedValue, "\n", ", "), a.FormattedType))
		}
	}
	line("Endereços", addrs)
	var rels []string
	for _, r := range p.Relations {
		rels = append(rels, typed(r.Person, r.FormattedType))
	}
	line("Relações", rels)
	var urls []string
	for _, u := range p.Urls {
		urls = append(urls, u.Value)
	}
	line("Links", urls)
	for _, bio := range p.Biographies {
		if s := strings.TrimSpace(bio.Value); s != "" {
			b.WriteString("\n" + s + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func (p *gPerson) birthday() string {
	for _, bd := range p.Birthdays {
		if bd.Date != nil && bd.Date.Month > 0 && bd.Date.Day > 0 {
			return fmt.Sprintf("%02d-%02d", bd.Date.Month, bd.Date.Day)
		}
	}
	return ""
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// SearchContacts implements agent.GoogleAPI: saved and "other" contacts are searched concurrently.
func (c *Client) SearchContacts(ctx context.Context, query string, max int) ([]agent.Contact, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("consulta vazia")
	}
	type endpoint struct {
		path, mask string
		other      bool
	}
	eps := []endpoint{{"people:searchContacts", searchMask, false}, {"otherContacts:search", "names,emailAddresses,phoneNumbers", true}}
	search := func(ctx context.Context, ep endpoint, q string) ([]agent.Contact, error) {
		v := url.Values{"query": {q}, "readMask": {ep.mask}, "pageSize": {strconv.Itoa(max)}}
		var resp struct {
			Results []struct {
				Person gPerson `json:"person"`
			} `json:"results"`
		}
		if err := c.do(ctx, http.MethodGet, peopleAPI+ep.path+"?"+v.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		out := make([]agent.Contact, 0, len(resp.Results))
		for _, r := range resp.Results {
			out = append(out, r.Person.contact(ep.other))
		}
		return out, nil
	}
	results := make([][]agent.Contact, len(eps))
	g, gctx := errgroup.WithContext(ctx)
	for i, ep := range eps {
		g.Go(func() error {
			if !c.warm.Load() { // the People API asks for an empty warm-up query first
				_, _ = search(gctx, ep, "")
			}
			var err error
			results[i], err = search(gctx, ep, query)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	c.warm.Store(true)
	seen := map[string]bool{}
	var out []agent.Contact
	for _, rs := range results {
		for _, ct := range rs {
			key := ct.Resource
			if len(ct.Emails) > 0 {
				key = ct.Emails[0]
			}
			if !seen[key] {
				seen[key] = true
				out = append(out, ct)
			}
		}
	}
	return out, nil
}

// SyncContacts mirrors saved contacts as person nodes (adopting auto-created ones).
func (s *Syncer) SyncContacts(ctx context.Context) (int, error) {
	if !s.g.Can(agent.GoogleContacts) {
		return 0, nil
	}
	v := url.Values{"personFields": {personFields}, "pageSize": {"1000"}, "sortOrder": {"LAST_MODIFIED_DESCENDING"}}
	count := 0
	for page := 0; page < 50; page++ {
		var resp struct {
			Connections   []gPerson `json:"connections"`
			NextPageToken string    `json:"nextPageToken"`
		}
		if err := s.g.do(ctx, http.MethodGet, peopleAPI+"people/me/connections?"+v.Encode(), nil, &resp); err != nil {
			return count, err
		}
		for i := range resp.Connections {
			changed, err := s.upsertContact(ctx, &resp.Connections[i])
			if err != nil {
				return count, err
			}
			if changed {
				count++
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		v.Set("pageToken", resp.NextPageToken)
	}
	return count, nil
}

func (s *Syncer) upsertContact(ctx context.Context, p *gPerson) (bool, error) {
	name := p.name()
	if name == "" || p.ResourceName == "" {
		return false, nil
	}
	content := p.markdown()
	hash := hashOf(name, content)
	db := s.ag.DB()
	var twin *database.Node
	if existing, err := db.GetNodeBySource(ctx, "contacts", p.ResourceName); err == nil {
		if h, _ := existing.Meta["hash"].(string); h == hash {
			return false, nil
		}
	} else if errors.Is(err, database.ErrNotFound) {
		// Adopt a person node auto-created by entity extraction; keep hand-written ones apart.
		if cand, err := db.FindByTitle(ctx, database.TypePerson, name); err == nil && cand.Source != "contacts" {
			if auto, _ := cand.Meta["auto"].(bool); auto || strings.TrimSpace(cand.Content) == "" {
				cand.Source, cand.SourceRef = "contacts", p.ResourceName
				if err := db.UpdateNode(ctx, cand); err != nil {
					return false, err
				}
			} else {
				twin = cand
			}
		}
	} else {
		return false, err
	}
	c := p.contact(false)
	meta := map[string]any{"emails": c.Emails, "phones": c.Phones, "hash": hash, "auto": false}
	if c.Company != "" {
		meta["company"] = c.Company
	}
	if bd := p.birthday(); bd != "" {
		meta["birthday"] = bd
	}
	n, _, err := s.ag.Ingest(ctx, agent.IngestInput{
		Type: database.TypePerson, Title: name, Content: content, Tags: []string{"contato"},
		Source: "contacts", SourceRef: p.ResourceName, Meta: meta, Enrich: true,
	})
	if errors.Is(err, database.ErrDeleted) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if twin != nil {
		_ = db.AddEdge(ctx, n.ID, twin.ID, "same_as", 1)
	}
	for _, e := range c.Emails { // events imported before this contact existed
		ids, err := db.NodesWithMetaValue(ctx, database.TypeEvent, "attendees", e)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			if err := db.AddEdge(ctx, id, n.ID, "attendee", 1); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}
