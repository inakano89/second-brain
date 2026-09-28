// Command server is the Second Brain single-binary, self-hosted server.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // embedded IANA timezone database (Windows / scratch images)

	"golang.org/x/term"

	"github.com/inakano89/second-brain/internal/agent"
	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/crypto"
	"github.com/inakano89/second-brain/internal/database"
	"github.com/inakano89/second-brain/internal/integrations/google"
	"github.com/inakano89/second-brain/internal/integrations/rss"
	"github.com/inakano89/second-brain/internal/integrations/zepp"
	"github.com/inakano89/second-brain/internal/llm"
	"github.com/inakano89/second-brain/internal/queue"
	"github.com/inakano89/second-brain/internal/scheduler"
	"github.com/inakano89/second-brain/internal/telegram"
	"github.com/inakano89/second-brain/internal/updater"
	"github.com/inakano89/second-brain/internal/watcher"
	"github.com/inakano89/second-brain/internal/web"
)

// Set at build time via -ldflags "-X main.version=... -X main.updatePublicKey=...".
var (
	version         = "dev"
	updatePublicKey = ""
)

func main() {
	envPath := flag.String("env", ".env", "caminho do arquivo .env")
	decrypt := flag.String("decrypt", "", "decifra um backup .db.enc e sai")
	out := flag.String("out", "", "arquivo de saída para -decrypt")
	key := flag.String("key", "", "chave do backup (padrão: BACKUP_ENCRYPTION_KEY do .env)")
	resetSetup := flag.Bool("reset-setup", false, "marca SETUP_COMPLETED=false para refazer o onboarding")
	showVersion := flag.Bool("version", false, "mostra a versão")
	updateNow := flag.Bool("update", false, "verifica e instala a última release do GitHub e sai")
	healthcheck := flag.Bool("healthcheck", false, "verifica se o servidor local responde (exit 0/1) — usado pelo Docker")
	resetPassword := flag.Bool("reset-password", false, "define uma nova senha de administrador e sai")
	flag.Parse()

	if *showVersion {
		fmt.Println("second-brain", version)
		return
	}
	cfg, err := config.Load(*envPath)
	if err != nil {
		fatal("falha ao ler .env", err)
	}
	if *decrypt != "" {
		runDecrypt(cfg, *decrypt, *out, *key)
		return
	}
	if *resetSetup {
		if err := cfg.Update(map[string]string{"SETUP_COMPLETED": "false"}); err != nil {
			fatal("falha ao gravar .env", err)
		}
		fmt.Println("Onboarding reativado. Inicie o servidor e acesse /setup.")
		return
	}

	if *healthcheck {
		os.Exit(runHealthcheck(cfg))
	}
	if *resetPassword {
		runResetPassword(cfg)
		return
	}

	exe := updater.Executable()
	if !*updateNow {
		// Container: prefer a newer binary installed by the auto-updater in the data volume.
		if p := updater.PreferOverlay(cfg, version, exe); p != "" {
			if err := updater.Relaunch(p, updater.FallbackEnv+"="+exe); err != nil {
				fmt.Fprintf(os.Stderr, "updater: não foi possível executar %s: %v\n", p, err)
			}
		}
		updater.Recover(exe, func(format string, a ...any) { fmt.Fprintf(os.Stderr, "updater: "+format+"\n", a...) })
	}

	level := slog.LevelInfo
	if os.Getenv("SB_DEBUG") != "" {
		level = slog.LevelDebug
	}
	console := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	dbPath := filepath.Join(cfg.GetPath("DATA_DIR"), "brain.db")
	db, err := database.Open(dbPath)
	if err != nil {
		fatal("falha ao abrir banco", err)
	}
	logHandler := database.NewLogHandler(db, console, slog.LevelInfo)
	log := slog.New(logHandler)
	slog.SetDefault(log)
	log.Info("Second Brain iniciando", "version", version, "env", cfg.Path(), "db", dbPath)

	llmMgr := llm.NewManager(cfg, func(provider, model, purpose string, u llm.Usage, cost float64) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = db.RecordUsage(ctx, database.UsageRecord{Provider: provider, Model: model, Purpose: purpose, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CostUSD: cost})
	})
	ag := agent.New(cfg, db, llmMgr, log)
	gc := google.New(cfg, db, log)
	ag.SetCalendar(gc)
	gsync := google.NewSyncer(gc, ag)
	tg := telegram.New(cfg, db, ag, log)
	zc := zepp.New(cfg, db, ag, log)
	rp := rss.New(cfg, ag, log)
	wt := watcher.New(cfg, db, ag, log)

	worker := queue.New(db, log)
	ag.RegisterTasks(worker)
	tg.RegisterTasks(worker)
	gsync.RegisterTasks(worker)
	zc.RegisterTasks(worker)
	rp.RegisterTasks(worker)
	wt.RegisterTasks(worker)

	upd := updater.New(cfg, db, log, version, updatePublicKey, exe)
	upd.Notify = tg.Notify
	if *updateNow {
		runUpdateCLI(upd)
		logHandler.Close()
		db.Close()
		return
	}
	restart := make(chan struct{}, 1)
	upd.RequestRestart = func() {
		select {
		case restart <- struct{}{}:
		default:
		}
	}

	deps := &scheduler.Deps{Cfg: cfg, DB: db, Agent: ag, Notifier: tg, PurgeTemp: tg.PurgeTemp, Update: upd.Run, Log: log.With("component", "routines")}
	tg.Briefing = func(ctx context.Context) (string, error) { return deps.MorningBriefing(ctx, false) }

	root, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sup := &supervisor{root: root, cfg: cfg, db: db, log: log, llm: llmMgr, ag: ag, worker: worker, tg: tg, wt: wt, deps: deps}
	sup.start()

	hr := &httpRunner{log: log.With("component", "http")}
	srv, err := web.New(web.Deps{
		Cfg: cfg, DB: db, LLM: llmMgr, Agent: ag, Google: gc, Zepp: zc, Telegram: tg, Updater: upd, Log: log, Version: version,
		Hooks: web.Hooks{Reload: sup.reload, Rebind: hr.rebind, Jobs: sup.jobs, RunJob: sup.runJob},
	})
	if err != nil {
		fatal("falha ao iniciar web", err)
	}
	hr.handler = srv.Handler()
	if err := hr.start(cfg.Addr()); err != nil {
		fatal("falha ao abrir porta HTTP", err)
	}
	if !cfg.SetupCompleted() {
		log.Info("primeiro acesso: abra o navegador para concluir o setup", "url", fmt.Sprintf("http://localhost:%d/setup", cfg.GetInt("HTTP_PORT", 8080)))
	}

	go upd.ConfirmAfter(root, 90*time.Second)

	relaunch := false
	select {
	case <-root.Done():
	case <-restart:
		relaunch = true
	}
	log.Info("encerrando…", "restart", relaunch)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hr.shutdown(shutdownCtx)
	sup.stop()
	logHandler.Close()
	db.Close()
	if relaunch {
		stop()
		next, env := upd.RelaunchPath(), []string(nil)
		if next != exe && os.Getenv(updater.FallbackEnv) == "" {
			env = append(env, updater.FallbackEnv+"="+exe)
		}
		if err := updater.Relaunch(next, env...); err != nil {
			fatal("falha ao reiniciar após atualização", err)
		}
	}
}

func runHealthcheck(cfg *config.Config) int {
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.GetInt("HTTP_PORT", 8080)))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func runResetPassword(cfg *config.Config) {
	if !cfg.SetupCompleted() {
		fatal("reset-password", errors.New("setup ainda não concluído: abra /setup no navegador"))
	}
	read := func(prompt string) string {
		fmt.Print(prompt)
		if term.IsTerminal(int(os.Stdin.Fd())) {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Println()
			if err != nil {
				fatal("reset-password", err)
			}
			return string(b)
		}
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(line, "\r\n")
	}
	pass := read("Nova senha (mín. 8 caracteres): ")
	if len([]rune(pass)) < 8 {
		fatal("reset-password", errors.New("senha muito curta"))
	}
	if term.IsTerminal(int(os.Stdin.Fd())) && read("Confirme: ") != pass {
		fatal("reset-password", errors.New("as senhas não conferem"))
	}
	hash, err := crypto.HashPassword(pass)
	if err != nil {
		fatal("reset-password", err)
	}
	if err := cfg.Update(map[string]string{"ADMIN_PASSWORD_HASH": hash}); err != nil {
		fatal("reset-password", err)
	}
	fmt.Printf("Senha do usuário %q alterada.\n", cfg.Get("ADMIN_USER"))
}

func runUpdateCLI(upd *updater.Updater) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	st, err := upd.Check(ctx)
	if err != nil {
		fatal("verificação de atualização", err)
	}
	if !st.Available {
		fmt.Printf("Já está na versão mais recente (%s; última release %s).\n", st.Current, st.Latest)
		return
	}
	if !st.Supported {
		fatal("atualização", errors.New(st.Reason))
	}
	if err := upd.Install(ctx); err != nil {
		fatal("atualização", err)
	}
	fmt.Printf("Atualizado para %s. Reinicie o serviço para aplicar.\n", st.Latest)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
	os.Exit(1)
}

func runDecrypt(cfg *config.Config, in, out, key string) {
	if key == "" {
		key = cfg.Get("BACKUP_ENCRYPTION_KEY")
	}
	if key == "" {
		fatal("decrypt", errors.New("informe -key ou defina BACKUP_ENCRYPTION_KEY"))
	}
	if out == "" {
		out = strings.TrimSuffix(in, ".enc")
		if out == in {
			out = in + ".db"
		}
	}
	if err := crypto.DecryptFile(in, out, key); err != nil {
		fatal("decrypt", err)
	}
	fmt.Println("Backup restaurado em", out)
}

// supervisor owns the background runners and restarts them on config changes.
type supervisor struct {
	root   context.Context
	cfg    *config.Config
	db     *database.DB
	log    *slog.Logger
	llm    *llm.Manager
	ag     *agent.Agent
	worker *queue.Worker
	tg     *telegram.Service
	wt     *watcher.Watcher
	deps   *scheduler.Deps

	reloadMu sync.Mutex
	mu       sync.Mutex
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	sched    *scheduler.Scheduler
}

func (s *supervisor) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil || !s.cfg.SetupCompleted() || s.root.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.root)
	s.cancel = cancel
	sched := scheduler.New(s.cfg.Location(), s.log)
	if err := scheduler.Register(sched, s.deps); err != nil {
		s.log.Error("agendamentos inválidos", "err", err)
	}
	s.sched = sched
	run := func(fn func(context.Context)) {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			fn(ctx)
		}()
	}
	run(func(ctx context.Context) { s.worker.Run(ctx, s.cfg.GetInt("QUEUE_WORKERS", 3)) })
	run(sched.Run)
	run(s.tg.Run)
	run(s.wt.Run)

	// Warm-up: embed anything missing (e.g. after switching embedding model) and pull integrations.
	for _, kind := range []string{agent.TaskReembed, google.TaskCalendarSync, zepp.TaskSync} {
		_, _ = s.db.Enqueue(ctx, kind, nil, database.EnqueueOpts{DedupeKey: kind, MaxAttempts: 3, Delay: 10 * time.Second})
	}
	s.log.Info("serviços em execução", "llm", len(s.llm.Providers()), "embeddings", s.llm.EmbedModel(), "telegram", s.tg.Enabled())
}

func (s *supervisor) stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		s.wg.Wait()
	}
}

// reload applies .env changes: rebuild LLM clients, reload actions, restart runners.
func (s *supervisor) reload() {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	s.llm.Reload(s.cfg)
	_ = s.ag.Actions().Reload()
	s.stop()
	s.start()
	s.log.Info("configuração recarregada")
}

func (s *supervisor) jobs() []scheduler.JobInfo {
	s.mu.Lock()
	sched := s.sched
	s.mu.Unlock()
	if sched == nil {
		return nil
	}
	return sched.Jobs()
}

func (s *supervisor) runJob(name string) error {
	s.mu.Lock()
	sched := s.sched
	s.mu.Unlock()
	if sched == nil {
		return errors.New("agendador inativo (setup pendente?)")
	}
	return sched.RunNow(name)
}

// httpRunner serves HTTP and can move to a new address without downtime.
type httpRunner struct {
	mu      sync.Mutex
	handler http.Handler
	srv     *http.Server
	addr    string
	log     *slog.Logger
}

func (h *httpRunner) serve(addr string) (*http.Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: h.handler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			h.log.Error("servidor HTTP parou", "err", err)
		}
	}()
	h.log.Info("HTTP escutando", "addr", addr)
	return srv, nil
}

func (h *httpRunner) start(addr string) error {
	srv, err := h.serve(addr)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.srv, h.addr = srv, addr
	h.mu.Unlock()
	return nil
}

func (h *httpRunner) rebind(addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if addr == h.addr {
		return
	}
	srv, err := h.serve(addr)
	if err != nil {
		h.log.Error("não foi possível migrar a porta HTTP; mantendo a atual", "addr", addr, "err", err)
		return
	}
	old := h.srv
	h.srv, h.addr = srv, addr
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = old.Shutdown(ctx)
	}()
}

func (h *httpRunner) shutdown(ctx context.Context) {
	h.mu.Lock()
	srv := h.srv
	h.mu.Unlock()
	if srv != nil {
		_ = srv.Shutdown(ctx)
	}
}
