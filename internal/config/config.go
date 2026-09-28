// Package config manages the .env file: parsing, typed access, locking and atomic persistence.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type line struct {
	raw string // comment/blank line verbatim
	key string // non-empty for KEY=VALUE lines
}

// Config is a concurrency-safe view over a .env file.
type Config struct {
	path   string
	mu     sync.RWMutex
	lines  []line
	values map[string]string

	subMu sync.Mutex
	subs  []func()
}

// Load reads the .env file at path. A missing file yields an empty config.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c := &Config{path: abs, values: map[string]string{}}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) reload() error {
	f, err := os.Open(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	lines, values, err := parse(f)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.lines, c.values = lines, values
	c.mu.Unlock()
	return nil
}

func parse(f *os.File) ([]line, map[string]string, error) {
	var lines []line
	values := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		raw := sc.Text()
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			lines = append(lines, line{raw: raw})
			continue
		}
		t = strings.TrimPrefix(t, "export ")
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			lines = append(lines, line{raw: "# " + raw})
			continue
		}
		k = strings.TrimSpace(k)
		v = unquote(strings.TrimSpace(v))
		if _, dup := values[k]; !dup {
			lines = append(lines, line{key: k})
		}
		values[k] = v
	}
	return lines, values, sc.Err()
}

func unquote(v string) string {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		s := v[1 : len(v)-1]
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteByte(s[i])
				}
				continue
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

func quote(v string) string {
	if v == "" {
		return ""
	}
	if !strings.ContainsAny(v, " \t\n\r\"'#\\$`=") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(v) + `"`
}

// Path returns the absolute .env path.
func (c *Config) Path() string { return c.path }

// Dir returns the directory holding the .env file.
func (c *Config) Dir() string { return filepath.Dir(c.path) }

// Exists reports whether the .env file exists on disk.
func (c *Config) Exists() bool {
	_, err := os.Stat(c.path)
	return err == nil
}

// Raw returns the value stored in the file (no defaults / env fallback).
func (c *Config) Raw(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.values[key]
	return v, ok
}

// Get resolves key: .env value, then process environment, then schema default.
func (c *Config) Get(key string) string {
	c.mu.RLock()
	v := c.values[key]
	c.mu.RUnlock()
	if v != "" {
		return v
	}
	if ev := os.Getenv(key); ev != "" {
		return ev
	}
	if f, ok := schemaIndex[key]; ok {
		return f.Default
	}
	return ""
}

// GetInt parses key as int with fallback.
func (c *Config) GetInt(key string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(c.Get(key))); err == nil {
		return n
	}
	return def
}

// GetFloat parses key as float with fallback.
func (c *Config) GetFloat(key string, def float64) float64 {
	if n, err := strconv.ParseFloat(strings.TrimSpace(c.Get(key)), 64); err == nil {
		return n
	}
	return def
}

// GetBool parses key as bool.
func (c *Config) GetBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(c.Get(key))) {
	case "1", "true", "yes", "on", "sim":
		return true
	}
	return false
}

// GetList splits by comma or newline, trimming empties.
func (c *Config) GetList(key string) []string {
	raw := strings.NewReplacer("\r", "", "\n", ",", ";", ",").Replace(c.Get(key))
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// GetPath resolves a path key relative to the .env directory.
func (c *Config) GetPath(key string) string {
	p := c.Get(key)
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir(), p)
}

// SetupCompleted reports whether onboarding finished.
func (c *Config) SetupCompleted() bool { return c.Exists() && c.GetBool("SETUP_COMPLETED") }

// Location returns the configured timezone.
func (c *Config) Location() *time.Location {
	if loc, err := time.LoadLocation(c.Get("TIMEZONE")); err == nil {
		return loc
	}
	return time.Local
}

// Addr returns host:port for the HTTP listener.
func (c *Config) Addr() string {
	return net.JoinHostPort(c.Get("HTTP_HOST"), strconv.Itoa(c.GetInt("HTTP_PORT", 8080)))
}

// PublicURL returns the external base URL without trailing slash.
func (c *Config) PublicURL() string {
	if u := strings.TrimRight(c.Get("PUBLIC_URL"), "/"); u != "" {
		return u
	}
	return fmt.Sprintf("http://localhost:%d", c.GetInt("HTTP_PORT", 8080))
}

// Keys returns every key known by schema or present in the file.
func (c *Config) Keys() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := map[string]bool{}
	var keys []string
	for _, f := range Schema {
		seen[f.Key] = true
		keys = append(keys, f.Key)
	}
	var extra []string
	for k := range c.values {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(keys, extra...)
}

// Update applies changes and persists atomically, then notifies subscribers.
func (c *Config) Update(changes map[string]string) error {
	if err := c.apply(changes); err != nil {
		return err
	}
	c.notify()
	return nil
}

func (c *Config) apply(changes map[string]string) error {
	unlock, err := c.lockFile()
	if err != nil {
		return err
	}
	defer unlock()
	// Re-read to merge concurrent external edits.
	if err := c.reload(); err != nil {
		return err
	}
	c.mu.Lock()
	for k, v := range changes {
		k = strings.TrimSpace(k)
		if k == "" || strings.ContainsAny(k, " =\n\t#") {
			continue
		}
		if _, ok := c.values[k]; !ok {
			c.lines = append(c.lines, line{key: k})
		}
		c.values[k] = v
	}
	data := c.render()
	c.mu.Unlock()
	return atomicWrite(c.path, data)
}

// Delete removes keys from the file.
func (c *Config) Delete(keys ...string) error {
	unlock, err := c.lockFile()
	if err != nil {
		return err
	}
	defer unlock()
	if err := c.reload(); err != nil {
		return err
	}
	c.mu.Lock()
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
		delete(c.values, k)
	}
	kept := c.lines[:0]
	for _, l := range c.lines {
		if l.key == "" || !drop[l.key] {
			kept = append(kept, l)
		}
	}
	c.lines = kept
	data := c.render()
	c.mu.Unlock()
	if err := atomicWrite(c.path, data); err != nil {
		return err
	}
	c.notify()
	return nil
}

func (c *Config) render() []byte {
	var b strings.Builder
	if len(c.lines) == 0 {
		b.WriteString("# Second Brain configuration — managed by the web UI\n")
	}
	for _, l := range c.lines {
		if l.key == "" {
			b.WriteString(l.raw)
		} else {
			b.WriteString(l.key + "=" + quote(c.values[l.key]))
		}
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// OnChange registers a callback fired after each successful Update.
func (c *Config) OnChange(fn func()) {
	c.subMu.Lock()
	c.subs = append(c.subs, fn)
	c.subMu.Unlock()
}

func (c *Config) notify() {
	c.subMu.Lock()
	subs := append([]func(){}, c.subs...)
	c.subMu.Unlock()
	for _, fn := range subs {
		go fn()
	}
}

// lockFile acquires a cross-process advisory lock (<path>.lock, O_EXCL).
func (c *Config) lockFile() (func(), error) {
	lock := c.path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d", os.Getpid())
			f.Close()
			return func() { os.Remove(lock) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(lock); serr == nil && time.Since(st.ModTime()) > 30*time.Second {
			os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("config: timeout acquiring %s", lock)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".env.tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	os.Chmod(name, 0o600)
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
