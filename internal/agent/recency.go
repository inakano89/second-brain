package agent

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/database"
)

// recencyFloor is the share of its score an item keeps however old it is: relevance stays the
// main signal and age only breaks ties between similarly relevant items.
const recencyFloor = 0.5

// defaultRecencyHalfLife is SEARCH_RECENCY_HALFLIFE (days) when the key is empty.
const defaultRecencyHalfLife = 365.0

// recencyHalfLife returns the half-life in days (0 = no age weighting).
func (a *Agent) recencyHalfLife() float64 {
	v := a.cfg.GetFloat("SEARCH_RECENCY_HALFLIFE", defaultRecencyHalfLife)
	if v < 0 {
		return 0
	}
	return v
}

// recencyFactor scales a search score by the age of the content: 1 for fresh items, falling
// towards recencyFloor with the given half-life (days). Open tasks, upcoming events, people and
// items with an unknown date are not aged.
func recencyFactor(n *database.Node, now time.Time, halfLifeDays float64) float64 {
	if halfLifeDays <= 0 || n.DateUnknown() || n.Type == database.TypePerson {
		return 1
	}
	if n.Type == database.TypeTask && n.Status != database.StatusDone {
		return 1
	}
	at := n.EffectiveAt()
	if n.Type == database.TypeEvent && n.DueAt != nil && at.After(now) {
		return 1
	}
	age := now.Sub(at).Hours() / 24
	if age <= 0 {
		return 1
	}
	return recencyFloor + (1-recencyFloor)*math.Pow(0.5, age/halfLifeDays)
}

var (
	yearRe    = regexp.MustCompile(`\b(19[89]\d|20\d\d)\b`)
	historyRe = regexp.MustCompile(`(?i)\b(antig[oa]s?|antigamente|no passado|anos atrás|há \d+ anos|hist[óo]ric[oa]|naquela [ée]poca|quando eu (era|morava|trabalhava|estudava))\b`)
)

// wantsHistory reports whether a question is about the past ("o que eu pensava em 2018?"): age
// weighting and the chat archive then step aside.
func wantsHistory(q string, now time.Time) bool {
	if historyRe.MatchString(q) {
		return true
	}
	for _, m := range yearRe.FindAllString(q, -1) {
		var y int
		for _, c := range m {
			y = y*10 + int(c-'0')
		}
		if y < now.Year() {
			return true
		}
	}
	return false
}

// archiveCutoff returns the date before which imported items stay out of the chat's automatic
// context (CHAT_ARCHIVE_YEARS; nil when the archive is off).
func (a *Agent) archiveCutoff(now time.Time) *time.Time {
	years := a.cfg.GetInt("CHAT_ARCHIVE_YEARS", 0)
	if years <= 0 {
		return nil
	}
	t := now.AddDate(-years, 0, 0)
	return &t
}

// ageLabel describes how old a piece of content is, for the model ("3 anos", "8 meses").
func ageLabel(at, now time.Time) string {
	d := now.Sub(at)
	switch days := int(d.Hours() / 24); {
	case days < 0:
		return "futuro"
	case days < 60:
		return "recente"
	case days < 730:
		return strconv.Itoa(days/30) + " meses"
	default:
		return strconv.Itoa(days/365) + " anos"
	}
}

// contextDates renders the date attributes of a retrieved node for the system prompt.
func contextDates(n *database.Node, loc *time.Location, now time.Time) string {
	var b strings.Builder
	switch {
	case n.DateUnknown():
		b.WriteString("data=desconhecida")
	case n.Type == database.TypeEvent && n.DueAt != nil:
		b.WriteString("acontece=" + n.DueAt.In(loc).Format("2006-01-02"))
	default:
		b.WriteString("criado=" + n.CreatedAt.In(loc).Format("2006-01-02"))
		if age := ageLabel(n.CreatedAt, now); strings.HasSuffix(age, "anos") || strings.HasSuffix(age, "meses") {
			b.WriteString(" idade=" + age)
		}
	}
	if at, ok := n.ImportedAt(); ok && n.Imported() && at.In(loc).Format("2006-01-02") != n.CreatedAt.In(loc).Format("2006-01-02") {
		b.WriteString(" importado=" + at.In(loc).Format("2006-01-02"))
	}
	return b.String()
}
