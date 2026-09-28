// Package zepp syncs daily health metrics from Zepp / Amazfit (Huami cloud API,
// unofficial) and accepts metrics pushed through a webhook.
package zepp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/crypto"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/queue"
)

// TaskSync is the queue kind for a Zepp pull.
const TaskSync = "zepp.sync"

// Metric kinds stored in the metrics table.
const (
	SleepMinutes  = "sleep_minutes"
	DeepSleep     = "deep_sleep_minutes"
	LightSleep    = "light_sleep_minutes"
	REMSleep      = "rem_sleep_minutes"
	SleepScore    = "sleep_score"
	RecoveryScore = "recovery_score"
	RestingHR     = "resting_hr"
	Steps         = "steps"
)

var units = map[string]string{
	SleepMinutes: "min", DeepSleep: "min", LightSleep: "min", REMSleep: "min",
	SleepScore: "pts", RecoveryScore: "pts", RestingHR: "bpm", Steps: "passos",
}

var labels = map[string]string{
	SleepMinutes: "Sono total", DeepSleep: "Sono profundo", LightSleep: "Sono leve", REMSleep: "REM",
	SleepScore: "Pontuação de sono", RecoveryScore: "Recuperação", RestingHR: "FC em repouso", Steps: "Passos",
}

// Client syncs Zepp data.
type Client struct {
	cfg  *config.Config
	db   *database.DB
	ag   *agent.Agent
	log  *slog.Logger
	http *http.Client
}

// New creates the Zepp client.
func New(cfg *config.Config, db *database.DB, ag *agent.Agent, log *slog.Logger) *Client {
	return &Client{cfg: cfg, db: db, ag: ag, log: log.With("component", "zepp"),
		http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Configured reports whether credentials exist.
func (c *Client) Configured() bool {
	return (c.cfg.Get("ZEPP_APP_TOKEN") != "" && c.cfg.Get("ZEPP_USER_ID") != "") || (c.cfg.Get("ZEPP_EMAIL") != "" && c.cfg.Get("ZEPP_PASSWORD") != "")
}

// RegisterTasks wires the queue handler.
func (c *Client) RegisterTasks(w *queue.Worker) {
	w.Handle(TaskSync, func(ctx context.Context, _ json.RawMessage) error {
		n, err := c.Sync(ctx, 3)
		if err == nil && n > 0 {
			c.log.Info("métricas Zepp sincronizadas", "days", n)
		}
		return err
	})
}

type session struct {
	AppToken string `json:"app_token"`
	UserID   string `json:"user_id"`
}

const sessionKey = "zepp.session"

func (c *Client) session(ctx context.Context, force bool) (*session, error) {
	if t, u := c.cfg.Get("ZEPP_APP_TOKEN"), c.cfg.Get("ZEPP_USER_ID"); t != "" && u != "" {
		return &session{AppToken: t, UserID: u}, nil
	}
	var s session
	if !force {
		if ok, _ := c.db.KVGetJSON(ctx, sessionKey, &s); ok && s.AppToken != "" {
			return &s, nil
		}
	}
	if err := c.login(ctx, &s); err != nil {
		return nil, err
	}
	return &s, c.db.KVSetJSON(ctx, sessionKey, s)
}

// login performs the (unofficial) Huami account flow: email/password → access code → app token.
func (c *Client) login(ctx context.Context, s *session) error {
	email, pass := c.cfg.Get("ZEPP_EMAIL"), c.cfg.Get("ZEPP_PASSWORD")
	if email == "" || pass == "" {
		return queue.Permanentf("zepp: configure ZEPP_APP_TOKEN+ZEPP_USER_ID ou ZEPP_EMAIL+ZEPP_PASSWORD")
	}
	form := url.Values{
		"state": {"REDIRECTION"}, "client_id": {"HuaMi"}, "password": {pass},
		"redirect_uri": {"https://s3-us-west-2.amazonaws.com/hm-registration/successsignin.html"},
		"region":       {"us-west-2"}, "token": {"access"}, "country_code": {"US"},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api-user.huami.com/registrations/"+url.PathEscape(email)+"/tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Query().Get("access") == "" {
		return queue.Permanentf("zepp: login recusado (HTTP %d) — verifique credenciais", resp.StatusCode)
	}
	form = url.Values{
		"app_name": {"com.xiaomi.hm.health"}, "app_version": {"6.3.5"}, "code": {loc.Query().Get("access")},
		"country_code": {"US"}, "device_id": {"02:00:00:" + crypto.RandomToken(3)[:2] + ":00:00"}, "device_model": {"android_phone"},
		"grant_type": {"access_token"}, "third_name": {"huami"}, "source": {"com.xiaomi.hm.health"}, "lang": {"en"}, "allow_registration": {"false"},
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, "https://account.huami.com/v2/client/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		TokenInfo struct {
			AppToken string `json:"app_token"`
			UserID   string `json:"user_id"`
		} `json:"token_info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if out.TokenInfo.AppToken == "" {
		return queue.Permanentf("zepp: app_token ausente na resposta de login")
	}
	s.AppToken, s.UserID = out.TokenInfo.AppToken, out.TokenInfo.UserID
	c.log.Info("login Zepp realizado")
	return nil
}

var errAuth = errors.New("zepp: token expirado")

// Sync pulls the last `days` of summaries.
func (c *Client) Sync(ctx context.Context, days int) (int, error) {
	if !c.Configured() {
		return 0, nil
	}
	s, err := c.session(ctx, false)
	if err != nil {
		return 0, err
	}
	n, err := c.fetch(ctx, s, days)
	if errors.Is(err, errAuth) && c.cfg.Get("ZEPP_APP_TOKEN") == "" {
		if s, err = c.session(ctx, true); err != nil {
			return 0, err
		}
		n, err = c.fetch(ctx, s, days)
	}
	return n, err
}

func (c *Client) fetch(ctx context.Context, s *session, days int) (int, error) {
	loc := c.cfg.Location()
	to := time.Now().In(loc)
	from := to.AddDate(0, 0, -days)
	q := url.Values{
		"query_type": {"summary"}, "device_type": {"android_phone"}, "userid": {s.UserID},
		"from_date": {from.Format("2006-01-02")}, "to_date": {to.Format("2006-01-02")},
	}
	base := strings.TrimRight(c.cfg.Get("ZEPP_API_BASE"), "/")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/data/band_data.json?"+q.Encode(), nil)
	req.Header.Set("apptoken", s.AppToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return 0, errAuth
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("zepp: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    []struct {
			DateTime string `json:"date_time"`
			Summary  string `json:"summary"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, err
	}
	if out.Code != 1 && out.Code != 0 {
		if strings.Contains(strings.ToLower(out.Message), "token") {
			return 0, errAuth
		}
		return 0, fmt.Errorf("zepp: código %d: %s", out.Code, out.Message)
	}
	count := 0
	for _, d := range out.Data {
		raw, err := base64.StdEncoding.DecodeString(d.Summary)
		if err != nil {
			continue
		}
		m := parseSummary(raw)
		if len(m) == 0 {
			continue
		}
		if err := c.Store(ctx, d.DateTime, m, "zepp"); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func num(m map[string]any, k string) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case string:
		var f float64
		fmt.Sscan(v, &f)
		return f
	}
	return 0
}

// parseSummary decodes a Huami daily summary blob.
func parseSummary(raw []byte) map[string]float64 {
	var s map[string]any
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	out := map[string]float64{}
	if slp, ok := s["slp"].(map[string]any); ok {
		deep, light, rem := num(slp, "dp"), num(slp, "lt"), num(slp, "rem")
		if total := deep + light + rem; total > 0 {
			out[SleepMinutes] = total
			out[DeepSleep] = deep
			out[LightSleep] = light
			if rem > 0 {
				out[REMSleep] = rem
			}
		}
		if v := num(slp, "ss"); v > 0 {
			out[SleepScore] = v
		}
		if v := num(slp, "rhr"); v > 0 {
			out[RestingHR] = v
		}
	}
	if stp, ok := s["stp"].(map[string]any); ok {
		if v := num(stp, "ttl"); v > 0 {
			out[Steps] = v
		}
	}
	return out
}

// Store persists metrics for date, estimates recovery if missing and upserts a daily health node.
func (c *Client) Store(ctx context.Context, date string, m map[string]float64, source string) error {
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return queue.Permanentf("data inválida %q", date)
	}
	if _, ok := m[RecoveryScore]; !ok {
		if r, ok := c.estimateRecovery(ctx, date, m); ok {
			m[RecoveryScore] = r
		}
	}
	for k, v := range m {
		if err := c.db.UpsertMetric(ctx, database.Metric{Date: date, Kind: k, Value: v, Unit: units[k], Source: source}); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	meta := map[string]any{"date": date}
	for _, k := range keys {
		label := labels[k]
		if label == "" {
			label = k
		}
		v := m[k]
		if k == SleepMinutes || k == DeepSleep || k == LightSleep || k == REMSleep {
			fmt.Fprintf(&b, "- **%s:** %dh%02d\n", label, int(v)/60, int(v)%60)
		} else {
			fmt.Fprintf(&b, "- **%s:** %.0f %s\n", label, v, units[k])
		}
		meta[k] = v
	}
	d, _ := time.ParseInLocation("2006-01-02", date, c.cfg.Location())
	_, _, err := c.ag.Ingest(ctx, agent.IngestInput{
		Type: database.TypeHealth, Title: "Saúde " + date, Content: b.String(), Tags: []string{"saude", source},
		Source: "health", SourceRef: date, CreatedAt: d, Meta: meta,
	})
	if errors.Is(err, database.ErrDeleted) {
		return nil
	}
	return err
}

// estimateRecovery derives a 0-100 readiness proxy from sleep and resting HR vs. 14-day baseline.
func (c *Client) estimateRecovery(ctx context.Context, date string, m map[string]float64) (float64, bool) {
	sleep, hasSleep := m[SleepMinutes]
	if !hasSleep {
		return 0, false
	}
	score := math.Min(sleep/480, 1.1) * 55
	if deep, ok := m[DeepSleep]; ok && sleep > 0 {
		score += math.Min(deep/sleep/0.2, 1) * 20
	} else {
		score += 10
	}
	if rhr, ok := m[RestingHR]; ok {
		d, _ := time.Parse("2006-01-02", date)
		avg, n, _ := c.db.MetricAverage(ctx, RestingHR, d.AddDate(0, 0, -14).Format("2006-01-02"), d.AddDate(0, 0, -1).Format("2006-01-02"))
		if n >= 3 && avg > 0 {
			score += math.Max(-15, math.Min(25, (avg-rhr)*5+15))
		} else {
			score += 15
		}
	} else {
		score += 15
	}
	return math.Round(math.Max(0, math.Min(100, score))), true
}
