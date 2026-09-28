package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/extract"
)

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// Action is a whitelisted host automation (see actions.example.json).
type Action struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Type        string            `json:"type"` // webhook | mqtt | command
	URL         string            `json:"url,omitempty"`
	Method      string            `json:"method,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body,omitempty"`
	Topic       string            `json:"topic,omitempty"`
	Payload     string            `json:"payload,omitempty"`
	Retain      bool              `json:"retain,omitempty"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	TimeoutSec  int               `json:"timeout_sec,omitempty"`
}

// Actions is the registry loaded from ACTIONS_FILE.
type Actions struct {
	cfg  *config.Config
	log  *slog.Logger
	mu   sync.RWMutex
	list []Action
	err  error
}

// NewActions loads the registry.
func NewActions(cfg *config.Config, log *slog.Logger) *Actions {
	a := &Actions{cfg: cfg, log: log.With("component", "actions")}
	a.Reload()
	return a
}

// Path returns the actions file path.
func (a *Actions) Path() string { return a.cfg.GetPath("ACTIONS_FILE") }

// Reload re-reads the actions file.
func (a *Actions) Reload() error {
	var list []Action
	b, err := os.ReadFile(a.Path())
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	} else if err == nil {
		err = json.Unmarshal(b, &list)
	}
	valid := list[:0]
	for _, x := range list {
		if x.Name == "" || (x.Type != "webhook" && x.Type != "mqtt" && x.Type != "command") {
			continue
		}
		valid = append(valid, x)
	}
	a.mu.Lock()
	a.list, a.err = valid, err
	a.mu.Unlock()
	if err != nil {
		a.log.Error("arquivo de ações inválido", "path", a.Path(), "err", err)
	}
	return err
}

// Enabled reports whether actions may run.
func (a *Actions) Enabled() bool { return a.cfg.GetBool("ACTIONS_ENABLED") }

// List returns the registered actions.
func (a *Actions) List() []Action {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]Action(nil), a.list...)
}

// Raw returns the file content for the editor.
func (a *Actions) Raw() string {
	b, err := os.ReadFile(a.Path())
	if err != nil {
		return "[]"
	}
	return string(b)
}

// Save validates and writes new file content.
func (a *Actions) Save(content string) error {
	var list []Action
	if err := json.Unmarshal([]byte(content), &list); err != nil {
		return fmt.Errorf("JSON inválido: %w", err)
	}
	pretty, _ := json.MarshalIndent(list, "", "  ")
	if err := os.WriteFile(a.Path(), pretty, 0o600); err != nil {
		return err
	}
	return a.Reload()
}

func (a *Actions) find(name string) (Action, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, x := range a.list {
		if x.Name == name {
			return x, true
		}
	}
	return Action{}, false
}

// Run executes an action by name, substituting {{input}}.
func (a *Actions) Run(ctx context.Context, name, input string) (string, error) {
	if !a.Enabled() {
		return "", errors.New("agent actions desativadas (ACTIONS_ENABLED=false)")
	}
	act, ok := a.find(name)
	if !ok {
		return "", fmt.Errorf("ação %q não permitida", name)
	}
	timeout := time.Duration(act.TimeoutSec) * time.Second
	if timeout <= 0 || timeout > 5*time.Minute {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sub := func(s string) string { return strings.ReplaceAll(s, "{{input}}", input) }
	var out string
	var err error
	switch act.Type {
	case "webhook":
		out, err = a.webhook(ctx, act, input, sub)
	case "mqtt":
		err = MQTTPublish(ctx, MQTTConfig{
			Broker: a.cfg.Get("MQTT_BROKER"), ClientID: a.cfg.Get("MQTT_CLIENT_ID"),
			Username: a.cfg.Get("MQTT_USERNAME"), Password: a.cfg.Get("MQTT_PASSWORD"),
		}, sub(act.Topic), []byte(sub(act.Payload)), act.Retain)
		out = "publicado em " + sub(act.Topic)
	case "command":
		args := make([]string, len(act.Args))
		for i, x := range act.Args {
			args[i] = sub(x) // passed as argv, never through a shell
		}
		var buf bytes.Buffer
		cmd := exec.CommandContext(ctx, act.Command, args...)
		cmd.Stdout, cmd.Stderr = &buf, &buf
		err = cmd.Run()
		out = buf.String()
	}
	a.log.Info("ação executada", "action", name, "type", act.Type, "ok", err == nil)
	if err != nil {
		return extract.Truncate(out, 4000), fmt.Errorf("ação %s: %w", name, err)
	}
	return extract.Truncate(out, 4000), nil
}

func (a *Actions) webhook(ctx context.Context, act Action, input string, sub func(string) string) (string, error) {
	method := strings.ToUpper(act.Method)
	if method == "" {
		method = http.MethodPost
	}
	u := strings.ReplaceAll(act.URL, "{{input}}", url.QueryEscape(input))
	var body io.Reader
	if act.Body != "" {
		body = strings.NewReader(sub(act.Body))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return "", err
	}
	if act.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range act.Headers {
		req.Header.Set(k, sub(v))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	out := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(b))
	if resp.StatusCode >= 400 {
		return out, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return out, nil
}
