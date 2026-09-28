// Package updater keeps the binary up to date from GitHub Releases:
// check → download → SHA-256 (+ optional ed25519 signature) verification →
// smoke test → DB snapshot → atomic swap → restart, with automatic rollback.
package updater

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/database"
)

const (
	statusKey   = "updater.status"
	notifiedKey = "updater.notified"
	maxAsset    = 150 << 20
)

// Status is the updater state shown in the UI.
type Status struct {
	Current     string    `json:"current"`
	Latest      string    `json:"latest"`
	Available   bool      `json:"available"`
	CheckedAt   time.Time `json:"checked_at"`
	PublishedAt time.Time `json:"published_at"`
	Notes       string    `json:"notes"`
	URL         string    `json:"url"`
	Error       string    `json:"error"`
	Installing  bool      `json:"installing"`
	Supported   bool      `json:"supported"`
	Reason      string    `json:"reason"`
	Enabled     bool      `json:"enabled"`
	Signed      bool      `json:"signed"`
	Asset       string    `json:"asset"`
}

type release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func (r *release) asset(name string) (string, int64, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, a.Size, true
		}
	}
	return "", 0, false
}

// Updater performs self-updates.
type Updater struct {
	cfg     *config.Config
	db      *database.DB
	log     *slog.Logger
	current string
	pubKey  ed25519.PublicKey
	exe     string
	http    *http.Client
	apiBase string      // overridable in tests
	inCtr   func() bool // container detection, overridable in tests

	// Notify sends a user notification (Telegram). Optional.
	Notify func(ctx context.Context, text string) error
	// RequestRestart asks main to shut down gracefully and relaunch. Optional.
	RequestRestart func()

	mu         sync.Mutex
	status     Status
	installing atomic.Bool
}

// New creates an updater. pubKey is a base64 ed25519 public key; when set,
// releases must carry a valid SHA256SUMS.sig.
func New(cfg *config.Config, db *database.DB, log *slog.Logger, current, pubKey, exe string) *Updater {
	u := &Updater{cfg: cfg, db: db, log: log.With("component", "updater"), current: current, exe: exe,
		http: &http.Client{Timeout: 10 * time.Minute}, apiBase: "https://api.github.com", inCtr: inContainer}
	if k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pubKey)); err == nil && len(k) == ed25519.PublicKeySize {
		u.pubKey = ed25519.PublicKey(k)
	}
	var st Status
	if db != nil {
		_, _ = db.KVGetJSON(context.Background(), statusKey, &st)
	}
	st.Current = current
	st.Installing = false
	if st.Latest != "" {
		st.Available = u.newer(st.Latest)
	}
	u.status = st
	return u
}

// Enabled reports whether automatic installation is on.
func (u *Updater) Enabled() bool { return u.cfg.GetBool("AUTO_UPDATE_ENABLED") }

// Status returns a snapshot of the current state.
func (u *Updater) Status() Status {
	u.mu.Lock()
	st := u.status
	u.mu.Unlock()
	st.Current = u.current
	st.Installing = u.installing.Load()
	st.Enabled = u.Enabled()
	st.Signed = u.pubKey != nil
	st.Asset = AssetName()
	st.Supported, st.Reason = u.Supported()
	return st
}

func (u *Updater) setStatus(fn func(*Status)) {
	u.mu.Lock()
	fn(&u.status)
	st := u.status
	u.mu.Unlock()
	if u.db != nil {
		_ = u.db.KVSetJSON(context.Background(), statusKey, st)
	}
}

// ---- versions ----

var semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)
var describeRe = regexp.MustCompile(`-\d+-g[0-9a-f]{6,}`)

type semver struct {
	n   [3]int
	pre string
	ok  bool
}

func parseVersion(v string) semver {
	m := semverRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return semver{}
	}
	var s semver
	for i := 0; i < 3; i++ {
		s.n[i], _ = strconv.Atoi(m[i+1])
	}
	s.pre, s.ok = m[4], true
	return s
}

// Compare returns -1, 0 or 1 comparing semantic versions a and b.
func Compare(a, b string) int {
	x, y := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		if x.n[i] != y.n[i] {
			if x.n[i] < y.n[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case x.pre == y.pre:
		return 0
	case x.pre == "":
		return 1
	case y.pre == "":
		return -1
	}
	xp, yp := strings.Split(x.pre, "."), strings.Split(y.pre, ".")
	for i := 0; i < len(xp) && i < len(yp); i++ {
		xi, xerr := strconv.Atoi(xp[i])
		yi, yerr := strconv.Atoi(yp[i])
		switch {
		case xerr == nil && yerr == nil && xi != yi:
			if xi < yi {
				return -1
			}
			return 1
		case (xerr == nil) != (yerr == nil):
			if xerr == nil {
				return -1 // numeric identifiers sort before alphanumeric
			}
			return 1
		case xp[i] != yp[i]:
			if xp[i] < yp[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(xp) < len(yp):
		return -1
	case len(xp) > len(yp):
		return 1
	}
	return 0
}

// IsRelease reports whether v is a tagged release (not a dev / git-describe build).
func IsRelease(v string) bool {
	return parseVersion(v).ok && !describeRe.MatchString(v) && !strings.Contains(v, "dirty")
}

func (u *Updater) newer(tag string) bool {
	return IsRelease(u.current) && parseVersion(tag).ok && Compare(tag, u.current) > 0 && !isSkipped(u.exe, tag)
}

// ---- platform ----

func goarm() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "GOARM" && s.Value != "" {
				return strings.SplitN(s.Value, ",", 2)[0]
			}
		}
	}
	return "7"
}

// AssetName is the release asset for this platform (matches the Makefile).
func AssetName() string {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv" + goarm()
	}
	n := "second-brain-" + runtime.GOOS + "-" + arch
	if runtime.GOOS == "windows" {
		n += ".exe"
	}
	return n
}

func inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	b, _ := os.ReadFile("/proc/1/cgroup")
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "kubepods") || strings.Contains(s, "containerd")
}

// Supported reports whether this process can replace its own binary.
func (u *Updater) Supported() (bool, string) {
	switch {
	case !IsRelease(u.current):
		return false, "build de desenvolvimento (" + u.current + "): use um binário de release"
	case u.inCtr():
		return false, "rodando em container: atualize a imagem (docker pull / Watchtower)"
	case u.exe == "":
		return false, "caminho do executável desconhecido"
	}
	f, err := os.CreateTemp(filepath.Dir(u.exe), ".sb-write-test-*")
	if err != nil {
		return false, "sem permissão de escrita em " + filepath.Dir(u.exe)
	}
	f.Close()
	os.Remove(f.Name())
	return true, ""
}

// ---- GitHub ----

func (u *Updater) repo() string {
	r := strings.Trim(u.cfg.Get("UPDATE_REPO"), "/ ")
	r = strings.TrimPrefix(r, "https://github.com/")
	return r
}

func (u *Updater) get(ctx context.Context, url string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "second-brain-updater/"+u.current)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %d %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (u *Updater) latest(ctx context.Context) (*release, error) {
	api := u.apiBase + "/repos/" + u.repo()
	prerelease := u.cfg.Get("UPDATE_CHANNEL") == "prerelease"
	url := api + "/releases/latest"
	if prerelease {
		url = api + "/releases?per_page=15"
	}
	resp, err := u.get(ctx, url, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if !prerelease {
		var r release
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return nil, err
		}
		return &r, nil
	}
	var list []release
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	var cands []release
	for _, r := range list {
		if !r.Draft && parseVersion(r.TagName).ok {
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		return nil, errors.New("nenhuma release encontrada")
	}
	sort.Slice(cands, func(i, j int) bool { return Compare(cands[i].TagName, cands[j].TagName) > 0 })
	return &cands[0], nil
}

// Check queries GitHub for the newest release.
func (u *Updater) Check(ctx context.Context) (Status, error) {
	rel, err := u.latest(ctx)
	if err != nil {
		u.setStatus(func(s *Status) { s.CheckedAt, s.Error = time.Now(), err.Error() })
		return u.Status(), err
	}
	u.setStatus(func(s *Status) {
		s.CheckedAt, s.Error = time.Now(), ""
		s.Latest, s.Notes, s.URL, s.PublishedAt = rel.TagName, rel.Body, rel.HTMLURL, rel.PublishedAt
		s.Available = u.newer(rel.TagName)
	})
	return u.Status(), nil
}

// Run is the scheduled job: check, then install (if enabled) or notify.
func (u *Updater) Run(ctx context.Context) error {
	st, err := u.Check(ctx)
	if err != nil {
		return err
	}
	if !st.Available {
		return nil
	}
	if st.Enabled && st.Supported {
		return u.Install(ctx)
	}
	if u.Notify != nil && u.db != nil {
		if last, _, _ := u.db.KVGet(ctx, notifiedKey); last != st.Latest {
			msg := fmt.Sprintf("⬆️ Nova versão do Second Brain: %s (atual %s).\n%s", st.Latest, st.Current, st.URL)
			if !st.Supported {
				msg += "\n" + st.Reason
			} else {
				msg += "\nInstalação automática desativada — instale em Configurações → Atualizações."
			}
			if u.Notify(ctx, msg) == nil {
				_ = u.db.KVSet(ctx, notifiedKey, st.Latest)
			}
		}
	}
	return nil
}

// ---- install ----

func parseSums(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && len(f[0]) == 64 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out
}

func (u *Updater) fetchSmall(ctx context.Context, url string) ([]byte, error) {
	resp, err := u.get(ctx, url, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// Install downloads, verifies and swaps the binary, then requests a restart.
func (u *Updater) Install(ctx context.Context) error {
	if ok, reason := u.Supported(); !ok {
		return errors.New(reason)
	}
	if !u.installing.CompareAndSwap(false, true) {
		return errors.New("atualização já em andamento")
	}
	defer u.installing.Store(false)
	err := u.install(ctx)
	if err != nil {
		u.log.Error("atualização falhou", "err", err)
		u.setStatus(func(s *Status) { s.Error = "instalação: " + err.Error() })
	}
	return err
}

func (u *Updater) install(ctx context.Context) error {
	rel, err := u.latest(ctx)
	if err != nil {
		return err
	}
	if !u.newer(rel.TagName) {
		return nil
	}
	name := AssetName()
	assetURL, size, ok := rel.asset(name)
	if !ok {
		return fmt.Errorf("release %s não contém %s", rel.TagName, name)
	}
	if size > maxAsset {
		return fmt.Errorf("asset grande demais (%d bytes)", size)
	}
	sumsURL, _, ok := rel.asset("SHA256SUMS")
	if !ok {
		return fmt.Errorf("release %s sem SHA256SUMS — recusando instalar", rel.TagName)
	}
	sums, err := u.fetchSmall(ctx, sumsURL)
	if err != nil {
		return err
	}
	if u.pubKey != nil {
		sigURL, _, ok := rel.asset("SHA256SUMS.sig")
		if !ok {
			return errors.New("release sem SHA256SUMS.sig e este binário exige assinatura")
		}
		sigB64, err := u.fetchSmall(ctx, sigURL)
		if err != nil {
			return err
		}
		sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
		if err != nil || !ed25519.Verify(u.pubKey, sums, sig) {
			return errors.New("assinatura ed25519 de SHA256SUMS inválida")
		}
	}
	want, ok := parseSums(sums)[name]
	if !ok {
		return fmt.Errorf("SHA256SUMS não lista %s", name)
	}

	u.log.Info("baixando atualização", "from", u.current, "to", rel.TagName, "asset", name)
	dir := filepath.Dir(u.exe)
	tmp, err := os.CreateTemp(dir, ".second-brain-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	resp, err := u.get(ctx, assetURL, "application/octet-stream")
	if err != nil {
		cleanup()
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxAsset))
	resp.Body.Close()
	if err != nil {
		cleanup()
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		cleanup()
		return fmt.Errorf("checksum não confere para %s (esperado %s, obtido %s)", name, want, got)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return err
	}

	// Smoke test: the new binary must run on this machine and report the expected version.
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	out, err := exec.CommandContext(sctx, tmpName, "-version").CombinedOutput()
	cancel()
	if err != nil || !strings.Contains(string(out), strings.TrimPrefix(rel.TagName, "v")) {
		os.Remove(tmpName)
		return fmt.Errorf("binário novo falhou no teste (-version): %v %s", err, strings.TrimSpace(string(out)))
	}

	if u.db != nil {
		snapDir := filepath.Join(u.cfg.GetPath("DATA_DIR"), "backups")
		if err := os.MkdirAll(snapDir, 0o700); err == nil {
			old, _ := filepath.Glob(filepath.Join(snapDir, "pre-update-*.db"))
			for _, f := range old {
				os.Remove(f)
			}
			snap := filepath.Join(snapDir, "pre-update-"+u.current+".db")
			if err := u.db.Snapshot(ctx, snap); err != nil {
				os.Remove(tmpName)
				return fmt.Errorf("snapshot pré-atualização: %w", err)
			}
		}
	}

	if err := swap(u.exe, tmpName); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := writeMarker(u.exe, marker{From: u.current, To: rel.TagName, At: time.Now()}); err != nil {
		u.log.Warn("falha ao gravar marcador de atualização", "err", err)
	}
	u.log.Info("atualização instalada, reiniciando", "from", u.current, "to", rel.TagName)
	if u.Notify != nil {
		_ = u.Notify(ctx, fmt.Sprintf("⬆️ Instalando Second Brain %s (era %s). Reiniciando…", rel.TagName, u.current))
	}
	if u.RequestRestart != nil {
		u.RequestRestart()
	}
	return nil
}

// swap replaces exe with next, keeping the previous binary as exe.old.
func swap(exe, next string) error {
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("renomear binário atual: %w", err)
	}
	if err := os.Rename(next, exe); err != nil {
		_ = os.Rename(old, exe)
		return fmt.Errorf("instalar binário novo: %w", err)
	}
	return nil
}
