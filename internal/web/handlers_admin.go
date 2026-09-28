package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/crypto"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/desktop"
	"github.com/inakano89/second-brain/internal/export"
	"github.com/inakano89/second-brain/internal/integrations/google"
	"github.com/inakano89/second-brain/internal/scheduler"
	"github.com/inakano89/second-brain/internal/telegram"
	"github.com/inakano89/second-brain/internal/updater"
)

// ---- dashboard ----

type dayBar struct {
	Day  string
	Cost float64
	Pct  float64
}

type integ struct {
	Name   string
	Status string
	OK     bool
}

type dashView struct {
	Days       int
	Usage      []database.UsageRow
	UsageTotal float64
	TokensIn   int64
	TokensOut  int64
	Daily      []dayBar
	Counts     []typeInfo
	Nodes      int
	Edges      int
	Vectors    map[string]int
	DBSize     string
	Queue      map[string]int
	Failed     []database.Task
	Jobs       []scheduler.JobInfo
	Briefing   *database.Node
	Metrics    []database.Metric
	Integs     []integ
}

func (s *Server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 365 {
		days = 30
	}
	since := time.Now().AddDate(0, 0, -days)
	v := dashView{Days: days, Vectors: s.DB.VectorCount(), DBSize: fmt.Sprintf("%.1f MB", float64(s.DB.Size())/1e6)}
	v.Usage, _ = s.DB.UsageSummary(ctx, since)
	for _, u := range v.Usage {
		v.UsageTotal += u.CostUSD
		v.TokensIn += u.InputTokens
		v.TokensOut += u.OutputTokens
	}
	daily, _ := s.DB.UsageDaily(ctx, time.Now().AddDate(0, 0, -14))
	perDay := map[string]float64{}
	var maxCost float64
	for _, d := range daily {
		perDay[d.Day] += d.CostUSD
	}
	for i := 13; i >= 0; i-- {
		day := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		v.Daily = append(v.Daily, dayBar{Day: day[5:], Cost: perDay[day]})
		maxCost = max(maxCost, perDay[day])
	}
	for i := range v.Daily {
		if maxCost > 0 {
			v.Daily[i].Pct = v.Daily[i].Cost / maxCost * 100
		}
	}
	counts, _ := s.DB.CountByType(ctx)
	for _, t := range database.NodeTypes {
		v.Counts = append(v.Counts, typeInfo{Type: t, Label: typeLabels[t], Color: template.CSS(typeColors[t]), Count: counts[t]})
		v.Nodes += counts[t]
	}
	v.Edges, _ = s.DB.EdgeCount(ctx)
	v.Queue, _ = s.DB.QueueStats(ctx)
	v.Failed, _ = s.DB.ListTasks(ctx, database.TaskFailed, 10)
	if s.Hooks.Jobs != nil {
		v.Jobs = s.Hooks.Jobs()
	}
	if id, ok, _ := s.DB.KVGet(ctx, "routine.latest.morning"); ok {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			v.Briefing, _ = s.DB.GetNode(ctx, n)
		}
	}
	loc := s.Cfg.Location()
	today := time.Now().In(loc)
	v.Metrics, _ = s.DB.MetricsRange(ctx, today.AddDate(0, 0, -7).Format("2006-01-02"), today.Format("2006-01-02"))
	v.Integs = s.integrations()
	s.render(w, "dashboard", s.page(r, "Painel", "dashboard", v))
}

func (s *Server) integrations() []integ {
	var out []integ
	provs := s.LLM.Providers()
	var names []string
	for _, p := range provs {
		n := p.Name + " (" + p.Model + ")"
		if p.Default {
			n += "★"
		}
		names = append(names, n)
	}
	out = append(out, integ{"LLM", firstOr(strings.Join(names, ", "), "nenhum provedor"), len(provs) > 0})
	out = append(out, integ{"Embeddings", s.LLM.EmbedModel(), true})
	tg := "desativado"
	if s.Telegram.Enabled() {
		tg = fmt.Sprintf("ativo, %d usuário(s) autorizado(s)", len(s.Telegram.AllowedIDs()))
	}
	out = append(out, integ{"Telegram", tg, s.Telegram.Enabled()})
	gs := "não configurado"
	if s.Google.Connected() {
		var on []string
		for _, st := range s.Google.Status() {
			if st.Enabled && st.Granted {
				on = append(on, st.Key)
			}
		}
		gs = "conectado: " + firstOr(strings.Join(on, ", "), "nenhum serviço autorizado")
		if s.Google.NeedsReconnect() {
			gs += " (reconecte para liberar o restante)"
		}
	} else if s.Google.Configured() {
		gs = "configurado, não conectado"
	}
	out = append(out, integ{"Google", gs, s.Google.Connected()})

	out = append(out, integ{"Zepp", map[bool]string{true: "configurado", false: "não configurado"}[s.Zepp.Configured()], s.Zepp.Configured()})
	feeds := s.Cfg.GetList("RSS_FEEDS")
	out = append(out, integ{"RSS", fmt.Sprintf("%d feed(s)", len(feeds)), len(feeds) > 0})
	out = append(out, integ{"Folder watcher", map[bool]string{true: s.Cfg.GetPath("INBOX_DIR"), false: "desativado"}[s.Cfg.GetBool("WATCHER_ENABLED")], s.Cfg.GetBool("WATCHER_ENABLED")})
	acts := s.Agent.Actions()
	out = append(out, integ{"Agent actions", fmt.Sprintf("%d ação(ões), %s", len(acts.List()), map[bool]string{true: "ativas", false: "desativadas"}[acts.Enabled()]), acts.Enabled()})
	if s.Updater != nil {
		st := s.Updater.Status()
		msg := st.Current
		switch {
		case st.Available:
			msg += " → " + st.Latest + " disponível"
		case !st.CheckedAt.IsZero():
			msg += " (atualizado)"
		}
		if !st.Enabled {
			msg += ", auto-update desligado"
		} else if !st.Supported {
			msg += ", " + st.Reason
		}
		out = append(out, integ{"Atualizações", msg, st.Enabled && st.Supported})
	}
	bk := s.Cfg.Get("BACKUP_ENCRYPTION_KEY") != ""
	out = append(out, integ{"Backup", strings.Join(s.Cfg.GetList("BACKUP_TARGETS"), ", "), bk})
	return out
}

func firstOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (s *Server) runJob(w http.ResponseWriter, r *http.Request) {
	if s.Hooks.RunJob == nil {
		redirectFlash(w, r, "/dashboard", "agendador indisponível", true)
		return
	}
	name := r.PathValue("name")
	if err := s.Hooks.RunJob(name); err != nil {
		redirectFlash(w, r, "/dashboard", err.Error(), true)
		return
	}
	redirectFlash(w, r, "/dashboard", "Job "+name+" iniciado.", false)
}

func (s *Server) queueRetry(w http.ResponseWriter, r *http.Request) {
	n, err := s.DB.RetryFailed(r.Context())
	if err != nil {
		redirectFlash(w, r, "/dashboard", err.Error(), true)
		return
	}
	redirectFlash(w, r, "/dashboard", fmt.Sprintf("%d tarefa(s) reenfileirada(s).", n), false)
}

// ---- logs ----

type logsView struct {
	Entries    []database.LogEntry
	Components []string
	Filter     database.LogFilter
	More       bool
	Last       int64
}

func (s *Server) logFilter(r *http.Request) database.LogFilter {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	return database.LogFilter{Level: q.Get("level"), Component: q.Get("component"), Query: q.Get("q"), Before: before, Limit: 100}
}

func (s *Server) loadLogs(r *http.Request) logsView {
	f := s.logFilter(r)
	entries, _ := s.DB.ListLogs(r.Context(), f)
	v := logsView{Entries: entries, Filter: f, More: len(entries) == f.Limit}
	if len(entries) > 0 {
		v.Last = entries[len(entries)-1].ID
	}
	return v
}

func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	v := s.loadLogs(r)
	v.Components, _ = s.DB.LogComponents(r.Context())
	s.render(w, "logs", s.page(r, "Audit log", "logs", v))
}

func (s *Server) logRows(w http.ResponseWriter, r *http.Request) {
	s.fragment(w, "log_rows", s.loadLogs(r))
}

// ---- settings ----

type envField struct {
	config.Field
	Value  string
	IsSet  bool
	Source string
}

type envGroup struct {
	Name   string
	Fields []envField
}

type backupFile struct {
	Name string
	Size string
	Date string
}

type settingsView struct {
	Groups      []envGroup
	Extra       string
	EnvPath     string
	GoogleOK    bool
	GoogleCfg   bool
	GoogleSvcs  []google.ServiceStatus
	GoogleNew   bool // enabled services still missing permissions
	Desktop     bool // Windows: login start and shutdown button
	Autostart   bool
	LogPath     string
	InboxDir    string
	RedirectURL string
	PublicURL   string
	APIToken    string
	Bookmarklet string
	ActionsRaw  string
	ActionsPath string
	Backups     []backupFile
	TelegramOK  bool
	Pending     []telegram.PendingUser
	Allowed     []int64
	Update      *updater.Status
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	byGroup := map[string][]envField{}
	known := map[string]bool{}
	for _, f := range config.Schema {
		known[f.Key] = true
		if f.Hidden {
			continue
		}
		raw, inFile := s.Cfg.Raw(f.Key)
		ef := envField{Field: f, IsSet: s.Cfg.Get(f.Key) != "" && (inFile || os.Getenv(f.Key) != "")}
		if !inFile && os.Getenv(f.Key) != "" {
			ef.Source = "env"
		}
		if !f.Secret {
			ef.Value = raw
			if ef.Value == "" && !inFile {
				ef.Value = s.Cfg.Get(f.Key)
			}
		}
		byGroup[f.Group] = append(byGroup[f.Group], ef)
	}
	v := settingsView{EnvPath: s.Cfg.Path(), GoogleOK: s.Google.Connected(), GoogleCfg: s.Google.Configured(), RedirectURL: s.Google.RedirectURL(),
		GoogleSvcs: s.Google.Status(), GoogleNew: s.Google.NeedsReconnect(), InboxDir: s.Cfg.GetPath("INBOX_DIR"),
		Desktop: desktop.Supported(), LogPath: filepath.Join(s.Cfg.GetPath("DATA_DIR"), "second-brain.log"),
		PublicURL: s.Cfg.PublicURL(), APIToken: s.Cfg.Get("API_TOKEN"), ActionsRaw: s.Agent.Actions().Raw(), ActionsPath: s.Agent.Actions().Path(), TelegramOK: s.Telegram.Enabled()}
	for _, g := range config.Groups {
		v.Groups = append(v.Groups, envGroup{Name: g, Fields: byGroup[g]})
	}
	var extra []string
	for _, k := range s.Cfg.Keys() {
		if !known[k] {
			val, _ := s.Cfg.Raw(k)
			extra = append(extra, k+"="+val)
		}
	}
	v.Extra = strings.Join(extra, "\n")
	v.Bookmarklet = bookmarklet(v.PublicURL, v.APIToken)
	v.Backups = listBackups(scheduler.BackupDir(s.Cfg))
	v.Pending = s.Telegram.Pending(r.Context())
	v.Allowed = s.Telegram.AllowedIDs()
	if s.Updater != nil {
		st := s.Updater.Status()
		v.Update = &st
	}
	_, v.Autostart, _ = desktop.Autostart()
	s.render(w, "settings", s.page(r, "Configurações", "settings", v))
}

func bookmarklet(base, token string) string {
	js := `(function(){var s=window.getSelection(),h='';if(s&&s.rangeCount){var d=document.createElement('div');for(var i=0;i<s.rangeCount;i++)d.appendChild(s.getRangeAt(i).cloneContents());h=d.innerHTML;}` +
		`var f=document.createElement('form');f.method='POST';f.action='` + base + `/api/clip';f.target='_blank';f.acceptCharset='UTF-8';` +
		`var a={url:location.href,title:document.title,html:h,token:'` + token + `'};for(var k in a){var e=document.createElement('input');e.type='hidden';e.name=k;e.value=a[k];f.appendChild(e);}` +
		`document.body.appendChild(f);f.submit();f.remove();})();`
	return "javascript:" + js
}

func listBackups(dir string) []backupFile {
	matches, _ := filepath.Glob(filepath.Join(dir, "second-brain-*.db.enc"))
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	var out []backupFile
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil {
			continue
		}
		out = append(out, backupFile{Name: filepath.Base(m), Size: fmt.Sprintf("%.1f MB", float64(st.Size())/1e6), Date: st.ModTime().Format("02/01/2006 15:04")})
	}
	return out
}

func (s *Server) settingsEnv(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectFlash(w, r, "/settings", err.Error(), true)
		return
	}
	changes := map[string]string{}
	rendered := map[string]bool{} // checkboxes present in the submitted form
	for _, k := range r.PostForm["__bool"] {
		rendered[k] = true
	}
	for _, f := range config.Schema {
		if f.Hidden {
			continue
		}
		if f.Kind == "bool" {
			if rendered[f.Key] {
				changes[f.Key] = strconv.FormatBool(r.PostFormValue(f.Key) == "true")
			}
			continue
		}
		vals, present := r.PostForm[f.Key]
		if !present {
			continue
		}
		v := strings.TrimSpace(vals[0])
		if f.Kind == "textarea" {
			v = strings.TrimSpace(strings.ReplaceAll(vals[0], "\r\n", "\n"))
		}
		if f.Secret {
			switch v {
			case "":
				continue // keep current
			case "!clear":
				v = ""
			}
		}
		changes[f.Key] = v
	}
	if tz := changes["TIMEZONE"]; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			redirectFlash(w, r, "/settings", "timezone inválida: "+tz, true)
			return
		}
	}
	for _, c := range []string{"CRON_MORNING", "CRON_EVENING", "CRON_WEEKLY", "CRON_MAINTENANCE", "CRON_BACKUP", "CRON_RSS", "CRON_GMAIL", "CRON_CALENDAR", "CRON_ZEPP", "CRON_UPDATE", "CRON_MODELS"} {
		if v := changes[c]; v != "" && v != "off" && v != "-" {
			if _, err := scheduler.Parse(v); err != nil {
				redirectFlash(w, r, "/settings", c+": "+err.Error(), true)
				return
			}
		}
	}
	known := map[string]bool{}
	for _, f := range config.Schema {
		known[f.Key] = true
	}
	newExtra := map[string]bool{}
	for _, line := range strings.Split(r.PostFormValue("__extra"), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || known[k] {
			continue
		}
		changes[k] = strings.TrimSpace(v)
		newExtra[k] = true
	}
	var removed []string
	for _, k := range s.Cfg.Keys() {
		if _, inFile := s.Cfg.Raw(k); inFile && !known[k] && !newExtra[k] {
			removed = append(removed, k)
		}
	}
	oldAddr := s.Cfg.Addr()
	if err := s.Cfg.Update(changes); err != nil {
		redirectFlash(w, r, "/settings", err.Error(), true)
		return
	}
	if len(removed) > 0 {
		_ = s.Cfg.Delete(removed...)
	}
	s.log.Info("configuração atualizada", "keys", len(changes))
	if s.Hooks.Reload != nil {
		go s.Hooks.Reload()
	}
	msg := "Configuração salva e aplicada."
	if newAddr := s.Cfg.Addr(); newAddr != oldAddr && s.Hooks.Rebind != nil {
		go func() { time.Sleep(time.Second); s.Hooks.Rebind(newAddr) }()
		msg = "Configuração salva. Servidor migrando para " + newAddr + "."
	}
	redirectFlash(w, r, "/settings", msg, false)
}

func (s *Server) settingsPassword(w http.ResponseWriter, r *http.Request) {
	if !crypto.VerifyPassword(r.FormValue("current"), s.Cfg.Get("ADMIN_PASSWORD_HASH")) {
		redirectFlash(w, r, "/settings", "senha atual incorreta", true)
		return
	}
	pass := r.FormValue("password")
	if err := requireLen(pass, 8, "a nova senha"); err != nil {
		redirectFlash(w, r, "/settings", err.Error(), true)
		return
	}
	if pass != r.FormValue("password2") {
		redirectFlash(w, r, "/settings", "as senhas não conferem", true)
		return
	}
	hash, err := crypto.HashPassword(pass)
	if err != nil {
		redirectFlash(w, r, "/settings", err.Error(), true)
		return
	}
	if err := s.Cfg.Update(map[string]string{"ADMIN_PASSWORD_HASH": hash}); err != nil {
		redirectFlash(w, r, "/settings", err.Error(), true)
		return
	}
	s.log.Info("senha alterada")
	redirectFlash(w, r, "/settings", "Senha alterada.", false)
}

func (s *Server) rotateToken(w http.ResponseWriter, r *http.Request) {
	if err := s.Cfg.Update(map[string]string{"API_TOKEN": crypto.RandomToken(24)}); err != nil {
		redirectFlash(w, r, "/settings", err.Error(), true)
		return
	}
	s.log.Info("API token rotacionado")
	redirectFlash(w, r, "/settings", "Novo API token gerado — atualize o bookmarklet.", false)
}

func (s *Server) settingsActions(w http.ResponseWriter, r *http.Request) {
	if err := s.Agent.Actions().Save(r.FormValue("actions")); err != nil {
		redirectFlash(w, r, "/settings", err.Error(), true)
		return
	}
	s.log.Info("ações atualizadas", "count", len(s.Agent.Actions().List()))
	redirectFlash(w, r, "/settings", "Ações salvas.", false)
}

func (s *Server) telegramTest(w http.ResponseWriter, r *http.Request) {
	if err := s.Telegram.Notify(r.Context(), "✅ Teste do "+s.Cfg.Get("BRAIN_NAME")+": notificações funcionando."); err != nil {
		redirectFlash(w, r, "/settings", "Telegram: "+err.Error(), true)
		return
	}
	redirectFlash(w, r, "/settings", "Mensagem de teste enviada.", false)
}

// ---- export & backup ----

func (s *Server) exportObsidian(w http.ResponseWriter, r *http.Request) {
	name := "second-brain-obsidian-" + time.Now().Format("20060102-1504") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if err := export.Obsidian(r.Context(), s.DB, w, s.Cfg.Location()); err != nil {
		s.log.Error("export Obsidian falhou", "err", err)
		return
	}
	s.log.Info("export Obsidian gerado")
}

func (s *Server) backupNow(w http.ResponseWriter, r *http.Request) {
	if s.Hooks.RunJob == nil {
		redirectFlash(w, r, "/settings", "agendador indisponível", true)
		return
	}
	if err := s.Hooks.RunJob("backup"); err != nil {
		redirectFlash(w, r, "/settings", err.Error(), true)
		return
	}
	redirectFlash(w, r, "/settings", "Backup iniciado — acompanhe no audit log.", false)
}

func (s *Server) backupDownload(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("file"))
	if !strings.HasPrefix(name, "second-brain-") || !strings.HasSuffix(name, ".db.enc") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, filepath.Join(scheduler.BackupDir(s.Cfg), name))
}

// ---- Google OAuth ----

func (s *Server) googleConnect(w http.ResponseWriter, r *http.Request) {
	if !s.Google.Configured() {
		redirectFlash(w, r, "/settings", "configure GOOGLE_CLIENT_ID e GOOGLE_CLIENT_SECRET", true)
		return
	}
	state := crypto.RandomToken(16)
	http.SetCookie(w, &http.Cookie{Name: "g_state", Value: state, Path: "/google", MaxAge: 600, HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.Google.AuthURL(state), http.StatusFound)
}

func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("g_state")
	if err != nil || !crypto.Equal(c.Value, r.URL.Query().Get("state")) {
		redirectFlash(w, r, "/settings", "estado OAuth inválido", true)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		redirectFlash(w, r, "/settings", "Google: "+e, true)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.Google.Exchange(ctx, r.URL.Query().Get("code")); err != nil {
		redirectFlash(w, r, "/settings", "Google: "+err.Error(), true)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "g_state", Value: "", Path: "/google", MaxAge: -1})
	s.enqueueGoogle(ctx)
	redirectFlash(w, r, "/settings#google", "Google conectado. A primeira sincronização começou em segundo plano.", false)
}

func (s *Server) enqueueGoogle(ctx context.Context) {
	for _, kind := range google.SyncTasks {
		_, _ = s.DB.Enqueue(ctx, kind, nil, database.EnqueueOpts{DedupeKey: kind, MaxAttempts: 4})
	}
}

func (s *Server) googleSync(w http.ResponseWriter, r *http.Request) {
	if !s.Google.Connected() {
		redirectFlash(w, r, "/settings#google", "Google não conectado.", true)
		return
	}
	s.enqueueGoogle(r.Context())
	redirectFlash(w, r, "/settings#google", "Sincronização do Google enfileirada.", false)
}

func (s *Server) googleDisconnect(w http.ResponseWriter, r *http.Request) {
	_ = s.Google.Disconnect(r.Context())
	redirectFlash(w, r, "/settings#google", "Google desconectado.", false)
}

// ---- desktop (Windows) ----

func (s *Server) autostartToggle(w http.ResponseWriter, r *http.Request) {
	_, on, err := desktop.Autostart()
	cmd := ""
	if err == nil && !on {
		cmd = desktop.Command(updater.Executable(), s.Cfg.Path())
	}
	if err == nil {
		err = desktop.SetAutostart(cmd)
	}
	if err != nil {
		redirectFlash(w, r, "/settings#desktop", "Início automático: "+err.Error(), true)
		return
	}
	s.log.Info("início automático alterado", "enabled", cmd != "")
	msg := "O Second Brain não vai mais iniciar com o Windows."
	if cmd != "" {
		msg = "Pronto: o Second Brain vai iniciar com o Windows, sem janela."
	}
	redirectFlash(w, r, "/settings#desktop", msg, false)
}

func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	if s.Hooks.Shutdown == nil || !desktop.Supported() {
		redirectFlash(w, r, "/settings", "encerrar pelo painel está disponível só no Windows", true)
		return
	}
	s.log.Info("encerramento solicitado pelo painel")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "<!doctype html><meta charset=utf-8><title>Second Brain</title><p style=\"font-family:sans-serif\">Second Brain encerrado. Para abrir de novo, use o atalho ou reinicie o computador.</p>")
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.Hooks.Shutdown()
	}()
}

// ---- self-update ----

func (s *Server) updateToggle(w http.ResponseWriter, r *http.Request) {
	on := !s.Cfg.GetBool("AUTO_UPDATE_ENABLED")
	if err := s.Cfg.Update(map[string]string{"AUTO_UPDATE_ENABLED": strconv.FormatBool(on)}); err != nil {
		redirectFlash(w, r, "/settings", err.Error(), true)
		return
	}
	s.log.Info("atualização automática alterada", "enabled", on)
	msg := "Atualizações automáticas desativadas."
	if on {
		msg = "Atualizações automáticas ativadas."
	}
	redirectFlash(w, r, "/settings#updates", msg, false)
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	if s.Updater == nil {
		redirectFlash(w, r, "/settings", "atualizador indisponível", true)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	st, err := s.Updater.Check(ctx)
	switch {
	case err != nil:
		redirectFlash(w, r, "/settings#updates", "Falha ao verificar: "+err.Error(), true)
	case st.Available:
		redirectFlash(w, r, "/settings#updates", "Nova versão disponível: "+st.Latest, false)
	default:
		redirectFlash(w, r, "/settings#updates", "Você está na versão mais recente ("+st.Current+").", false)
	}
}

func (s *Server) updateInstall(w http.ResponseWriter, r *http.Request) {
	if s.Updater == nil {
		redirectFlash(w, r, "/settings", "atualizador indisponível", true)
		return
	}
	if ok, reason := s.Updater.Supported(); !ok {
		redirectFlash(w, r, "/settings#updates", reason, true)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		_ = s.Updater.Install(ctx)
	}()
	redirectFlash(w, r, "/settings#updates", "Instalando atualização — o servidor reiniciará em instantes.", false)
}
