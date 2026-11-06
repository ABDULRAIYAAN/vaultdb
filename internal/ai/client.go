//go:build !js

// HTTP plumbing shared by the provider adapters. Left out of the browser
// build, where VaultDB only uses the local embedder.

package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func (c Config) resolve(embedding bool) (Provider, string, string, error) {
	p, ok := Providers[strings.ToLower(c.Provider)]
	if !ok {
		return p, "", "", fmt.Errorf("unknown provider %q (choose from: %s)", c.Provider, strings.Join(ProviderNames(), ", "))
	}
	model := c.Model
	if model == "" {
		if embedding {
			model = p.EmbedModel
		} else {
			model = p.ChatModel
		}
	}
	what := "chat"
	if embedding {
		what = "embedding"
	}
	if model == "" {
		if p.Kind == "openai" && (embedding && p.EmbedModel == "" && p.ChatModel != "" || !embedding && p.ChatModel == "" && p.EmbedModel != "") {
			return p, "", "", fmt.Errorf("%s has no %s models; choose another provider for that part", p.Name, what)
		}
		if p.Kind == "local" || p.Kind == "none" || p.Kind == "anthropic" {
			return p, "", "", fmt.Errorf("%s has no %s models; choose another provider for that part", p.Name, what)
		}
		return p, "", "", fmt.Errorf("%s needs a %s model name (use -model or -embed-model)", p.Name, what)
	}
	if c.BaseURL != "" {
		p.BaseURL = strings.TrimRight(c.BaseURL, "/")
	}
	key := c.APIKey
	if key == "" && p.KeyEnv != "" {
		key = os.Getenv(p.KeyEnv)
		if key == "" {
			return p, "", "", fmt.Errorf("%s needs an API key: set the %s environment variable (or put it in vaultdb-chat.env)", p.Name, p.KeyEnv)
		}
	}
	return p, model, key, nil
}

// NewEmbedder builds the embedder described by c.
func NewEmbedder(c Config) (Embedder, error) {
	p, model, key, err := c.resolve(true)
	if err != nil {
		return nil, err
	}
	switch p.Kind {
	case "local":
		return NewHashEmbedder(512), nil
	case "openai":
		return &openAI{http: newHTTP(), base: p.BaseURL, key: key, model: model, name: p.Name}, nil
	case "gemini":
		return &gemini{http: newHTTP(), base: p.BaseURL, key: key, model: model}, nil
	}
	return nil, fmt.Errorf("%s has no embedding models; choose another provider for that part", p.Name)
}

// NewChatModel builds the chat model described by c. It returns nil (and no
// error) for the "none" provider.
func NewChatModel(c Config) (ChatModel, error) {
	if strings.EqualFold(c.Provider, "none") {
		return nil, nil
	}
	p, model, key, err := c.resolve(false)
	if err != nil {
		return nil, err
	}
	switch p.Kind {
	case "openai":
		return &openAI{http: newHTTP(), base: p.BaseURL, key: key, model: model, name: p.Name}, nil
	case "gemini":
		return &gemini{http: newHTTP(), base: p.BaseURL, key: key, model: model}, nil
	case "anthropic":
		return &anthropic{http: newHTTP(), base: p.BaseURL, key: key, model: model}, nil
	}
	return nil, fmt.Errorf("%s has no chat models; choose another provider for that part", p.Name)
}

func newHTTP() *http.Client { return &http.Client{Timeout: 3 * time.Minute} }

// postJSON sends body as JSON and decodes the JSON reply into out. Error
// replies are turned into readable messages (the API key is never included).
func postJSON(ctx context.Context, c *http.Client, url string, headers map[string]string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return fmt.Errorf("could not reach %s: is the program running?", req.URL.Host)
		}
		return fmt.Errorf("request failed: %w", redactKey(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 400 {
			msg = msg[:400] + "..."
		}
		return fmt.Errorf("%s replied %s: %s", req.URL.Host, resp.Status, msg)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unexpected reply from %s: %v", req.URL.Host, err)
	}
	return nil
}

// redactKey removes ?key=... from error messages (Gemini puts the key in the URL).
func redactKey(err error) error {
	s := err.Error()
	if i := strings.Index(s, "key="); i >= 0 {
		end := strings.IndexAny(s[i:], "&\" ")
		if end < 0 {
			end = len(s) - i
		}
		s = s[:i] + "key=REDACTED" + s[i+end:]
	}
	return errors.New(s)
}
