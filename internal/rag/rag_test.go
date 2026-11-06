package rag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ABDULRAIYAAN/vaultdb/internal/ai"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

func TestSplit(t *testing.T) {
	if got := Split("  short text  ", 100, 10); len(got) != 1 || got[0] != "short text" {
		t.Fatalf("short: %q", got)
	}
	if got := Split("", 100, 10); got != nil {
		t.Fatalf("empty: %q", got)
	}
	para := strings.Repeat("word ", 50) // 250 chars
	text := para + "\n\n" + para + "\n\n" + para
	chunks := Split(text, 300, 40)
	if len(chunks) < 3 {
		t.Fatalf("expected at least 3 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len([]rune(c)) > 300 {
			t.Errorf("chunk %d has %d chars", i, len([]rune(c)))
		}
		if strings.HasPrefix(c, "ord") || strings.HasSuffix(c, "wor") {
			t.Errorf("chunk %d cuts a word: %q...", i, c[:20])
		}
	}
	// Nothing is lost: every word of the text appears in some chunk.
	joined := strings.Join(chunks, " ")
	if strings.Count(joined, "word") < 150 {
		t.Fatal("words lost while splitting")
	}
}

// fakeChat records what it was sent and answers with a fixed reply.
type fakeChat struct{ last []ai.Message }

func (f *fakeChat) Name() string { return "fake/chat" }
func (f *fakeChat) Chat(_ context.Context, _ string, msgs []ai.Message) (string, error) {
	f.last = msgs
	return "Nodes split when full [1].", nil
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIngestAndAsk(t *testing.T) {
	docs := t.TempDir()
	writeFiles(t, docs, map[string]string{
		"btree.md":       "A B-tree keeps keys sorted. When a node is full, the node splits in two and the middle key moves up.",
		"notes/cake.txt": "Chocolate cake needs flour, sugar, cocoa and eggs. Bake for thirty minutes.",
		"notes/hnsw.md":  "HNSW is a layered graph used for approximate nearest neighbour search.",
		"image.png":      "not text",
		".git/config":    "hidden folder, must be skipped",
	})
	st := store.New()
	chat := &fakeChat{}
	r := New(st, ai.NewHashEmbedder(512), chat, "docs")

	rep, err := r.Ingest(context.Background(), docs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 3 || rep.Skipped != 1 || rep.Chunks != 3 {
		t.Fatalf("report %+v", rep)
	}

	ans, err := r.Ask(context.Background(), "what happens when a B-tree node is full?", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Sources[0].File != "btree.md" {
		t.Fatalf("best source = %s, want btree.md", ans.Sources[0].File)
	}
	if ans.Text != "Nodes split when full [1]." {
		t.Fatalf("answer %q", ans.Text)
	}
	prompt := chat.last[len(chat.last)-1].Content
	if !strings.Contains(prompt, "[1] (from btree.md)") || !strings.Contains(prompt, "Question: what happens") {
		t.Fatalf("prompt did not include numbered sources:\n%s", prompt)
	}

	// Running again changes nothing.
	rep, _ = r.Ingest(context.Background(), docs, nil)
	if rep.Unchanged != 3 || rep.Chunks != 0 {
		t.Fatalf("second run %+v", rep)
	}

	// Edit one file, delete another.
	writeFiles(t, docs, map[string]string{"btree.md": "B-trees are also used by SQLite and Postgres indexes."})
	os.Remove(filepath.Join(docs, "notes/cake.txt"))
	rep, _ = r.Ingest(context.Background(), docs, nil)
	if rep.Updated != 1 || rep.Removed != 1 || rep.Unchanged != 1 {
		t.Fatalf("after edits %+v", rep)
	}
	ans, _ = r.Ask(context.Background(), "chocolate cake cocoa eggs", 5, nil)
	for _, s := range ans.Sources {
		if s.File == "notes/cake.txt" {
			t.Fatal("deleted file still returned")
		}
	}
}

func TestPassagesOnlyWithoutChatModel(t *testing.T) {
	docs := t.TempDir()
	writeFiles(t, docs, map[string]string{"a.md": "VaultDB stores data in an append-only log."})
	r := New(store.New(), ai.NewHashEmbedder(256), nil, "docs")
	r.Ingest(context.Background(), docs, nil)
	ans, err := r.Ask(context.Background(), "append-only log", 3, nil)
	if err != nil || ans.Text != "" || len(ans.Sources) != 1 {
		t.Fatalf("answer %+v, %v", ans, err)
	}
}

func TestRefusesToMixEmbeddingModels(t *testing.T) {
	docs := t.TempDir()
	writeFiles(t, docs, map[string]string{"a.md": "some text"})
	st := store.New()
	New(st, ai.NewHashEmbedder(512), nil, "docs").Ingest(context.Background(), docs, nil)

	other := New(st, ai.NewHashEmbedder(256), nil, "docs")
	if _, err := other.Ask(context.Background(), "q", 1, nil); err == nil || !strings.Contains(err.Error(), "was built with local/hash-512") {
		t.Fatalf("Ask with another model: %v", err)
	}
	writeFiles(t, docs, map[string]string{"b.md": "more text"})
	if _, err := other.Ingest(context.Background(), docs, nil); err == nil {
		t.Fatal("Ingest with another model should fail")
	}
}

func TestAskBeforeIngest(t *testing.T) {
	r := New(store.New(), ai.NewHashEmbedder(64), nil, "docs")
	if _, err := r.Ask(context.Background(), "q", 1, nil); err == nil || !strings.Contains(err.Error(), "ingest") {
		t.Fatalf("err = %v", err)
	}
}

func TestEmptyFolder(t *testing.T) {
	r := New(store.New(), ai.NewHashEmbedder(64), nil, "docs")
	if _, err := r.Ingest(context.Background(), t.TempDir(), nil); err == nil {
		t.Fatal("empty folder should be reported")
	}
}
