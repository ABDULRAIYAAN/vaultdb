// Package ai talks to embedding models and chat models from many providers
// through two small interfaces, so the rest of VaultDB never cares which
// company (or which program on your laptop) is behind them.
//
// Most providers speak the "OpenAI-compatible" API (same URLs, same JSON),
// so one adapter covers OpenAI, Groq, Mistral, OpenRouter, Voyage, Jina,
// Ollama and LM Studio. Gemini and Anthropic have their own formats and get
// their own adapters. A built-in "local" embedder needs no model at all.
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
	"sort"
	"strings"
	"time"
)

// Embedder turns texts into vectors (one per text, same order).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Name() string // provider/model, recorded so a collection is never mixed
}

// Message is one turn of a conversation.
type Message struct {
	Role    string // "user" or "assistant"
	Content string
}

// ChatModel writes a reply given a system instruction and the conversation.
type ChatModel interface {
	Chat(ctx context.Context, system string, msgs []Message) (string, error)
	Name() string
}

// Provider describes one service and its sensible defaults.
type Provider struct {
	Name       string
	Kind       string // "openai", "gemini", "anthropic", "local", "none"
	BaseURL    string
	KeyEnv     string // environment variable holding the API key ("" = no key)
	ChatModel  string // default chat model ("" = has no chat models)
	EmbedModel string // default embedding model ("" = has no embeddings)
	Note       string
}

// Providers is every supported service. Model names are defaults; any model
// the provider offers can be chosen with -model / -embed-model.
var Providers = map[string]Provider{
	"local": {Name: "local", Kind: "local", EmbedModel: "hash-512",
		Note: "built in, no setup: word-based embeddings, good for trying things out"},
	"none": {Name: "none", Kind: "none", ChatModel: "-",
		Note: "no AI answer: just show the best matching passages"},
	"ollama": {Name: "ollama", Kind: "openai", BaseURL: "http://localhost:11434/v1",
		ChatModel: "llama3.2", EmbedModel: "nomic-embed-text",
		Note: "free, runs on your computer (ollama.com); run 'ollama pull <model>' first"},
	"lmstudio": {Name: "lmstudio", Kind: "openai", BaseURL: "http://localhost:1234/v1",
		Note: "free, runs on your computer (lmstudio.ai); pass the loaded model's name with -model / -embed-model"},
	"openai": {Name: "openai", Kind: "openai", BaseURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY",
		ChatModel: "gpt-5-mini", EmbedModel: "text-embedding-3-small"},
	"gemini": {Name: "gemini", Kind: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta", KeyEnv: "GEMINI_API_KEY",
		ChatModel: "gemini-flash-latest", EmbedModel: "gemini-embedding-001",
		Note: "free tier available (aistudio.google.com)"},
	"anthropic": {Name: "anthropic", Kind: "anthropic", BaseURL: "https://api.anthropic.com/v1", KeyEnv: "ANTHROPIC_API_KEY",
		ChatModel: "claude-haiku-5-5", Note: "chat only; pair with any embedding provider"},
	"groq": {Name: "groq", Kind: "openai", BaseURL: "https://api.groq.com/openai/v1", KeyEnv: "GROQ_API_KEY",
		ChatModel: "llama-3.3-70b-versatile", Note: "very fast, free tier; chat only"},
	"mistral": {Name: "mistral", Kind: "openai", BaseURL: "https://api.mistral.ai/v1", KeyEnv: "MISTRAL_API_KEY",
		ChatModel: "mistral-small-latest", EmbedModel: "mistral-embed"},
	"openrouter": {Name: "openrouter", Kind: "openai", BaseURL: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY",
		ChatModel: "openrouter/auto", Note: "hundreds of models behind one key; chat only"},
	"voyage": {Name: "voyage", Kind: "openai", BaseURL: "https://api.voyageai.com/v1", KeyEnv: "VOYAGE_API_KEY",
		EmbedModel: "voyage-3.5", Note: "embeddings only"},
	"jina": {Name: "jina", Kind: "openai", BaseURL: "https://api.jina.ai/v1", KeyEnv: "JINA_API_KEY",
		EmbedModel: "jina-embeddings-v3", Note: "embeddings only"},
}

// ProviderNames returns the provider names, sorted.
func ProviderNames() []string {
	var names []string
	for n := range Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Config picks a provider and optionally overrides its model, URL or key.
type Config struct {
	Provider string
	Model    string // "" = provider default
	BaseURL  string // "" = provider default
	APIKey   string // "" = read the provider's environment variable
}

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
