package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
)

// OpenAI implements Provider/Embedder/Transcriber for the OpenAI API and any
// OpenAI-compatible server (Ollama, llama.cpp, LM Studio, vLLM).
type OpenAI struct {
	name            string
	baseURL         string
	apiKey          string
	model           string
	embedModel      string
	transcribeModel string
	local           bool
}

// NewOpenAI builds the official OpenAI client.
func NewOpenAI(baseURL, apiKey, model, embedModel, transcribeModel string) *OpenAI {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAI{name: "openai", baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model, embedModel: embedModel, transcribeModel: transcribeModel}
}

// NewLocal builds a client for an OpenAI-compatible local server.
func NewLocal(baseURL, model, embedModel string) *OpenAI {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}
	return &OpenAI{name: "ollama", baseURL: baseURL, model: model, embedModel: embedModel, local: true}
}

// Name implements Provider.
func (o *OpenAI) Name() string { return o.name }

// Model implements Provider.
func (o *OpenAI) Model() string { return o.model }

// EmbedModel implements Embedder.
func (o *OpenAI) EmbedModel() string { return o.name + ":" + o.embedModel }

func (o *OpenAI) headers() map[string]string {
	h := map[string]string{}
	if o.apiKey != "" {
		h["Authorization"] = "Bearer " + o.apiKey
	}
	return h
}

func dataURI(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func (o *OpenAI) convert(req Request) []map[string]any {
	var out []map[string]any
	if req.System != "" {
		out = append(out, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			out = append(out, map[string]any{"role": "system", "content": m.TextOf()})
		case RoleTool:
			out = append(out, map[string]any{"role": "tool", "tool_call_id": m.ToolCallID, "content": m.Content})
		case RoleAssistant:
			msg := map[string]any{"role": "assistant"}
			if m.Content != "" {
				msg["content"] = m.Content
			} else {
				msg["content"] = nil
			}
			if len(m.ToolCalls) > 0 {
				var calls []map[string]any
				for _, c := range m.ToolCalls {
					args := string(c.Arguments)
					if args == "" {
						args = "{}"
					}
					calls = append(calls, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": args}})
				}
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
		default:
			if len(m.Parts) == 0 {
				out = append(out, map[string]any{"role": "user", "content": m.Content})
				continue
			}
			var content []map[string]any
			if m.Content != "" {
				content = append(content, map[string]any{"type": "text", "text": m.Content})
			}
			for _, p := range m.Parts {
				switch p.Type {
				case PartText:
					content = append(content, map[string]any{"type": "text", "text": p.Text})
				case PartImage:
					content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURI(p.MIME, p.Data)}})
				case PartFile:
					if strings.HasPrefix(p.MIME, "text/") {
						content = append(content, map[string]any{"type": "text", "text": string(p.Data)})
						continue
					}
					name := p.Name
					if name == "" {
						name = "document.pdf"
					}
					content = append(content, map[string]any{"type": "file", "file": map[string]any{"filename": name, "file_data": dataURI(p.MIME, p.Data)}})
				case PartAudio:
					format := ""
					switch {
					case strings.Contains(p.MIME, "wav"):
						format = "wav"
					case strings.Contains(p.MIME, "mpeg"), strings.Contains(p.MIME, "mp3"):
						format = "mp3"
					}
					if format != "" {
						content = append(content, map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": base64.StdEncoding.EncodeToString(p.Data), "format": format}})
					}
				}
			}
			out = append(out, map[string]any{"role": "user", "content": content})
		}
	}
	return out
}

type oaChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Stream implements Provider.
func (o *OpenAI) Stream(ctx context.Context, req Request, onText StreamFunc) (*Response, error) {
	model := req.Model
	if model == "" {
		model = o.model
	}
	body := map[string]any{
		"model":          model,
		"messages":       o.convert(req),
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if o.local {
		body["max_tokens"] = defaultMax(req.MaxTokens)
	} else {
		body["max_completion_tokens"] = defaultMax(req.MaxTokens)
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			params := t.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": params}})
		}
		body["tools"] = tools
	} else if req.JSON {
		body["response_format"] = map[string]any{"type": "json_object"}
	}
	resp, err := post(ctx, o.name, o.baseURL+"/chat/completions", o.headers(), body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &Response{Provider: o.name, Model: model}
	var text strings.Builder
	type acc struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*acc{}
	err = readSSE(resp.Body, func(_, data string) error {
		if data == "[DONE]" {
			return nil
		}
		var ch oaChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return nil
		}
		if ch.Error != nil {
			return &APIError{Provider: o.name, Status: 500, Body: ch.Error.Message}
		}
		if ch.Usage != nil {
			out.Usage = Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
		}
		for _, c := range ch.Choices {
			if c.Delta.Content != "" {
				text.WriteString(c.Delta.Content)
				if onText != nil {
					if err := onText(c.Delta.Content); err != nil {
						return err
					}
				}
			}
			for _, tc := range c.Delta.ToolCalls {
				a := calls[tc.Index]
				if a == nil {
					a = &acc{}
					calls[tc.Index] = a
				}
				if tc.ID != "" {
					a.id = tc.ID
				}
				if tc.Function.Name != "" {
					a.name += tc.Function.Name
				}
				a.args.WriteString(tc.Function.Arguments)
			}
			if c.FinishReason != "" {
				out.StopReason = c.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Text = text.String()
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		a := calls[i]
		args := a.args.String()
		if !json.Valid([]byte(args)) {
			args = "{}"
		}
		id := a.id
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: id, Name: a.name, Arguments: json.RawMessage(args)})
	}
	if out.Usage.InputTokens == 0 && out.Usage.OutputTokens == 0 {
		in := 0
		for _, m := range req.Messages {
			in += estimateTokens(m.TextOf())
		}
		out.Usage = Usage{InputTokens: in + estimateTokens(req.System), OutputTokens: estimateTokens(out.Text)}
	}
	return out, nil
}

// Embed implements Embedder (batched).
func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, Usage, error) {
	var all [][]float32
	var usage Usage
	for start := 0; start < len(texts); start += 64 {
		end := min(start+64, len(texts))
		var resp struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if err := postJSON(ctx, o.name, o.baseURL+"/embeddings", o.headers(), map[string]any{"model": o.embedModel, "input": texts[start:end]}, &resp); err != nil {
			return nil, usage, err
		}
		batch := make([][]float32, end-start)
		for _, d := range resp.Data {
			if d.Index >= 0 && d.Index < len(batch) {
				batch[d.Index] = d.Embedding
			}
		}
		all = append(all, batch...)
		usage.InputTokens += resp.Usage.PromptTokens
	}
	return all, usage, nil
}

// Transcribe implements Transcriber via /audio/transcriptions.
func (o *OpenAI) Transcribe(ctx context.Context, audio []byte, filename, mime string) (string, float64, error) {
	model := o.transcribeModel
	if model == "" {
		model = "whisper-1"
	}
	verbose := strings.HasPrefix(model, "whisper")
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return "", 0, err
	}
	fw.Write(audio)
	w.WriteField("model", model)
	if verbose {
		w.WriteField("response_format", "verbose_json")
	} else {
		w.WriteField("response_format", "json")
	}
	w.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return "", 0, &APIError{Provider: o.name, Status: resp.StatusCode, Body: string(b)}
	}
	var out struct {
		Text     string  `json:"text"`
		Duration float64 `json:"duration"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, err
	}
	seconds := out.Duration
	if seconds == 0 {
		seconds = float64(len(audio)) / 2000 // ~16 kbps opus estimate
	}
	return strings.TrimSpace(out.Text), seconds / 60 * 0.006, nil
}
