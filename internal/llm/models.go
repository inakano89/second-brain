package llm

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

var nonChat = []string{"embed", "tts", "whisper", "dall-e", "audio", "realtime", "transcribe", "image", "moderation", "search", "davinci", "babbage", "computer-use", "codex"}

func chatLike(id string) bool {
	l := strings.ToLower(id)
	for _, x := range nonChat {
		if strings.Contains(l, x) {
			return false
		}
	}
	return true
}

// ListModels queries the provider API for available chat model ids.
func (m *Manager) ListModels(ctx context.Context, provider string) ([]string, error) {
	m.mu.RLock()
	p, ok := m.providers[provider]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotConfigured, provider)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var ids []string
	switch c := p.(type) {
	case *OpenAI:
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := getJSON(ctx, c.name, c.baseURL+"/models", c.headers(), &out); err != nil {
			return nil, err
		}
		for _, d := range out.Data {
			if c.local || ((strings.HasPrefix(d.ID, "gpt-") || strings.HasPrefix(d.ID, "o")) && chatLike(d.ID)) {
				ids = append(ids, d.ID)
			}
		}
	case *Anthropic:
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		h := map[string]string{"x-api-key": c.apiKey, "anthropic-version": "2023-06-01"}
		if err := getJSON(ctx, "anthropic", c.baseURL+"/models?limit=100", h, &out); err != nil {
			return nil, err
		}
		for _, d := range out.Data {
			ids = append(ids, d.ID)
		}
	case *Gemini:
		var out struct {
			Models []struct {
				Name    string   `json:"name"`
				Methods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
		}
		if err := getJSON(ctx, "gemini", c.baseURL+"/models?pageSize=200", c.headers(), &out); err != nil {
			return nil, err
		}
		for _, d := range out.Models {
			id := strings.TrimPrefix(d.Name, "models/")
			gen := false
			for _, mt := range d.Methods {
				if mt == "generateContent" {
					gen = true
				}
			}
			if gen && strings.HasPrefix(id, "gemini") && chatLike(id) {
				ids = append(ids, id)
			}
		}
	default:
		return nil, fmt.Errorf("listagem não suportada para %s", provider)
	}
	sort.Strings(ids)
	return ids, nil
}
