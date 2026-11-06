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
	"context"
	"sort"
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
