//go:build !js

package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ---- OpenAI-compatible (OpenAI, Groq, Mistral, OpenRouter, Voyage, Jina, Ollama, LM Studio)

type openAI struct {
	http        *http.Client
	base, key   string
	model, name string
}

func (o *openAI) Name() string { return o.name + "/" + o.model }

func (o *openAI) headers() map[string]string {
	if o.key == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + o.key}
}

func (o *openAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	var resp struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	body := map[string]any{"model": o.model, "input": texts}
	if err := postJSON(ctx, o.http, o.base+"/embeddings", o.headers(), body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("asked for %d embeddings, got %d", len(texts), len(resp.Data))
	}
	out := make([][]float32, len(texts))
	for i, d := range resp.Data {
		idx := d.Index
		if idx < 0 || idx >= len(out) {
			idx = i
		}
		out[idx] = d.Embedding
	}
	return out, nil
}

func (o *openAI) Chat(ctx context.Context, system string, msgs []Message) (string, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	all := []msg{{"system", system}}
	for _, m := range msgs {
		all = append(all, msg{m.Role, m.Content})
	}
	var resp struct {
		Choices []struct {
			Message msg `json:"message"`
		} `json:"choices"`
	}
	if err := postJSON(ctx, o.http, o.base+"/chat/completions", o.headers(),
		map[string]any{"model": o.model, "messages": all}, &resp); err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("the model returned no answer")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

// ---- Google Gemini

type gemini struct {
	http      *http.Client
	base, key string
	model     string
}

func (g *gemini) Name() string { return "gemini/" + g.model }

func (g *gemini) url(method string) string {
	return fmt.Sprintf("%s/models/%s:%s?key=%s", g.base, url.PathEscape(g.model), method, url.QueryEscape(g.key))
}

func (g *gemini) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Parts []part `json:"parts"`
	}
	type request struct {
		Model   string  `json:"model"`
		Content content `json:"content"`
	}
	reqs := make([]request, len(texts))
	for i, t := range texts {
		reqs[i] = request{Model: "models/" + g.model, Content: content{Parts: []part{{t}}}}
	}
	var resp struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err := postJSON(ctx, g.http, g.url("batchEmbedContents"), nil, map[string]any{"requests": reqs}, &resp); err != nil {
		return nil, err
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("asked for %d embeddings, got %d", len(texts), len(resp.Embeddings))
	}
	out := make([][]float32, len(texts))
	for i, e := range resp.Embeddings {
		out[i] = e.Values
	}
	return out, nil
}

func (g *gemini) Chat(ctx context.Context, system string, msgs []Message) (string, error) {
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}
	var contents []content
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model" // Gemini's name for the assistant
		}
		contents = append(contents, content{Role: role, Parts: []part{{m.Content}}})
	}
	var resp struct {
		Candidates []struct {
			Content content `json:"content"`
		} `json:"candidates"`
	}
	body := map[string]any{"systemInstruction": content{Parts: []part{{system}}}, "contents": contents}
	if err := postJSON(ctx, g.http, g.url("generateContent"), nil, body, &resp); err != nil {
		return "", err
	}
	if len(resp.Candidates) == 0 {
		return "", errors.New("the model returned no answer (it may have been blocked by safety filters)")
	}
	var b strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String()), nil
}

// ---- Anthropic

type anthropic struct {
	http      *http.Client
	base, key string
	model     string
}

func (a *anthropic) Name() string { return "anthropic/" + a.model }

func (a *anthropic) Chat(ctx context.Context, system string, msgs []Message) (string, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var all []msg
	for _, m := range msgs {
		all = append(all, msg{m.Role, m.Content})
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	headers := map[string]string{"x-api-key": a.key, "anthropic-version": "2023-06-01"}
	body := map[string]any{"model": a.model, "max_tokens": 1024, "system": system, "messages": all}
	if err := postJSON(ctx, a.http, a.base+"/messages", headers, body, &resp); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	if b.Len() == 0 {
		return "", errors.New("the model returned no answer")
	}
	return strings.TrimSpace(b.String()), nil
}
