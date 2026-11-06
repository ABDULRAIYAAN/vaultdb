//go:build !js

package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fake starts a server that checks each request and answers with reply.
func fake(t *testing.T, check func(r *http.Request, body map[string]any), reply string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		check(r, body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestOpenAICompatible(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "secret-groq")
	url := fake(t, func(r *http.Request, body map[string]any) {
		if r.Header.Get("Authorization") != "Bearer secret-groq" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/chat/completions" || body["model"] != "llama-3.3-70b-versatile" {
			t.Errorf("path %s model %v", r.URL.Path, body["model"])
		}
		msgs := body["messages"].([]any)
		if first := msgs[0].(map[string]any); first["role"] != "system" || len(msgs) != 2 {
			t.Errorf("messages %v", msgs)
		}
	}, `{"choices":[{"message":{"role":"assistant","content":"  Hello [1]  "}}]}`)

	m, err := NewChatModel(Config{Provider: "groq", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Chat(context.Background(), "be brief", []Message{{"user", "hi"}})
	if err != nil || got != "Hello [1]" {
		t.Fatalf("Chat = %q, %v", got, err)
	}
}

func TestOpenAIEmbeddingsKeepOrder(t *testing.T) {
	url := fake(t, func(r *http.Request, body map[string]any) {
		if r.URL.Path != "/embeddings" || len(body["input"].([]any)) != 2 {
			t.Errorf("path %s input %v", r.URL.Path, body["input"])
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("ollama needs no key, none should be sent")
		}
	}, `{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`) // out of order on purpose

	e, err := NewEmbedder(Config{Provider: "ollama", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	vs, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil || vs[0][0] != 1 || vs[1][1] != 1 {
		t.Fatalf("Embed = %v, %v", vs, err)
	}
	if e.Name() != "ollama/nomic-embed-text" {
		t.Fatalf("Name = %s", e.Name())
	}
}

func TestGemini(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gem-key")
	url := fake(t, func(r *http.Request, body map[string]any) {
		if r.URL.Query().Get("key") != "gem-key" {
			t.Error("key missing")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/models/gemini-embedding-001:batchEmbedContents"):
			reqs := body["requests"].([]any)
			if reqs[0].(map[string]any)["model"] != "models/gemini-embedding-001" {
				t.Errorf("embed request %v", reqs[0])
			}
		case strings.HasSuffix(r.URL.Path, "/models/gemini-flash-latest:generateContent"):
			contents := body["contents"].([]any)
			if contents[1].(map[string]any)["role"] != "model" { // assistant becomes "model"
				t.Errorf("roles %v", contents)
			}
			if body["systemInstruction"] == nil {
				t.Error("system instruction missing")
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}, `{"embeddings":[{"values":[0.5,0.5]}],"candidates":[{"content":{"parts":[{"text":"Part one. "},{"text":"Part two."}]}}]}`)

	e, err := NewEmbedder(Config{Provider: "gemini", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	if vs, err := e.Embed(context.Background(), []string{"x"}); err != nil || len(vs[0]) != 2 {
		t.Fatalf("Embed = %v, %v", vs, err)
	}
	m, _ := NewChatModel(Config{Provider: "gemini", BaseURL: url})
	got, err := m.Chat(context.Background(), "sys", []Message{{"user", "q1"}, {"assistant", "a1"}, {"user", "q2"}})
	if err != nil || got != "Part one. Part two." {
		t.Fatalf("Chat = %q, %v", got, err)
	}
}

func TestAnthropic(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "ant-key")
	url := fake(t, func(r *http.Request, body map[string]any) {
		if r.URL.Path != "/messages" || r.Header.Get("x-api-key") != "ant-key" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("path %s headers %v", r.URL.Path, r.Header)
		}
		if body["system"] != "sys" || body["max_tokens"] == nil {
			t.Errorf("body %v", body)
		}
	}, `{"content":[{"type":"text","text":"Answer from the model [2]"}]}`)
	m, err := NewChatModel(Config{Provider: "anthropic", BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.Chat(context.Background(), "sys", []Message{{"user", "q"}}); err != nil || got != "Answer from the model [2]" {
		t.Fatalf("Chat = %q, %v", got, err)
	}
}

func TestErrorsAreReadableAndHideKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	m, _ := NewChatModel(Config{Provider: "openai", BaseURL: srv.URL, APIKey: "sk-secret"})
	_, err := m.Chat(context.Background(), "", []Message{{"user", "hi"}})
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "model not found") || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("error = %v", err)
	}
	if got := redactKey(errSimple("GET https://x/y?key=abc123&alt=json: timeout")).Error(); strings.Contains(got, "abc123") {
		t.Fatalf("key not redacted: %s", got)
	}
}

type errSimple string

func (e errSimple) Error() string { return string(e) }

func TestConfigErrors(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	cases := []struct {
		chat bool
		cfg  Config
		want string
	}{
		{true, Config{Provider: "nope"}, "unknown provider"},
		{false, Config{Provider: "groq"}, "has no embedding models"},
		{true, Config{Provider: "voyage"}, "has no chat models"},
		{false, Config{Provider: "anthropic"}, "has no embedding models"},
		{true, Config{Provider: "local"}, "has no chat models"},
		{true, Config{Provider: "lmstudio"}, "needs a chat model name"},
		{true, Config{Provider: "openai"}, "set the OPENAI_API_KEY"},
	}
	for _, c := range cases {
		var err error
		if c.chat {
			_, err = NewChatModel(c.cfg)
		} else {
			_, err = NewEmbedder(c.cfg)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v (chat=%v): error %v, want %q", c.cfg, c.chat, err, c.want)
		}
	}
	if m, err := NewChatModel(Config{Provider: "none"}); m != nil || err != nil {
		t.Fatal("none should give no model and no error")
	}
}

func TestHashEmbedderPrefersSharedWords(t *testing.T) {
	e := NewHashEmbedder(512)
	vs, _ := e.Embed(context.Background(), []string{
		"B-tree nodes split when full",
		"How do B-tree nodes split?",
		"Chocolate cake recipe with cream",
	})
	dot := func(a, b []float32) (s float32) {
		for i := range a {
			s += a[i] * b[i]
		}
		return
	}
	if dot(vs[0], vs[1]) <= dot(vs[0], vs[2]) {
		t.Fatal("texts sharing words should be closer")
	}
}
