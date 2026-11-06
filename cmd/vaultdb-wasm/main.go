//go:build js && wasm

// Command vaultdb-wasm is VaultDB compiled to WebAssembly, so the website can
// run the real engine inside each visitor's browser tab with no server.
//
// It adds one global object to the page:
//
//	vaultdb.exec(line)  -> {reply, micros, keys, bytes}
//	vaultdb.reset()     -> {reply}
//	vaultdb.ask(q)      -> {sources: [{n, file, text}]} or {error}
//
// Build: GOOS=js GOARCH=wasm go build -ldflags="-s -w" -o web/src/public/vaultdb.wasm ./cmd/vaultdb-wasm
package main

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"syscall/js"
	"time"

	vaultdb "github.com/ABDULRAIYAAN/vaultdb"
	"github.com/ABDULRAIYAAN/vaultdb/internal/ai"
	"github.com/ABDULRAIYAAN/vaultdb/internal/engine"
	"github.com/ABDULRAIYAAN/vaultdb/internal/rag"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

const (
	maxDataBytes = 8 << 20 // keeps a runaway loop from eating the tab's memory
	maxReply     = 64 << 10
	maxLine      = 64 << 10
)

var (
	st   = store.New()
	eng  = engine.New(st)
	docs *rag.RAG
)

func main() {
	api := map[string]any{
		"exec":  js.FuncOf(exec),
		"reset": js.FuncOf(reset),
		"ask":   js.FuncOf(ask),
	}
	js.Global().Set("vaultdb", js.ValueOf(api))
	js.Global().Call("dispatchEvent", js.Global().Get("Event").New("vaultdb-ready"))
	select {} // keep the functions alive
}

func exec(_ js.Value, args []js.Value) any {
	if len(args) == 0 {
		return result("(error) nothing to run")
	}
	line := args[0].String()
	if len(line) > maxLine {
		return result(fmt.Sprintf("(error) commands are limited to %d KB", maxLine>>10))
	}
	start := time.Now()
	reply := eng.ExecuteLine(line)
	micros := time.Since(start).Microseconds()

	s := st.Stats()
	if s.LiveBytes > maxDataBytes {
		st.Clear()
		eng = engine.New(st)
		reply = fmt.Sprintf("(error) the database went over its %d MB limit, so it was emptied", maxDataBytes>>20)
		s = st.Stats()
	}
	if len(reply) > maxReply {
		reply = reply[:maxReply] + "\n... (reply cut short)"
	}
	return map[string]any{"reply": reply, "micros": micros, "keys": s.Keys, "bytes": s.LiveBytes}
}

func reset(js.Value, []js.Value) any {
	st.Clear()
	eng = engine.New(st)
	return result("OK, your database is empty again")
}

func result(reply string) map[string]any {
	s := st.Stats()
	return map[string]any{"reply": reply, "micros": 0, "keys": s.Keys, "bytes": s.LiveBytes}
}

// ask searches the embedded docs. Answers are the passages themselves: a
// chat model would need an API key, and keys can't be hidden in a web page.
func ask(_ js.Value, args []js.Value) any {
	if len(args) == 0 || strings.TrimSpace(args[0].String()) == "" {
		return map[string]any{"error": "type a question first"}
	}
	q := args[0].String()
	if len(q) > 500 {
		return map[string]any{"error": "questions are limited to 500 characters"}
	}
	ctx := context.Background()
	if docs == nil {
		r := rag.New(store.New(), ai.NewHashEmbedder(512), nil, "docs")
		if _, err := r.IngestFS(ctx, noTests{vaultdb.Docs}, nil); err != nil {
			return map[string]any{"error": "could not read the docs: " + err.Error()}
		}
		docs = r
	}
	ans, err := docs.Ask(ctx, q, 4, nil)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var sources []any
	for _, s := range ans.Sources {
		sources = append(sources, map[string]any{"n": s.N, "file": s.File, "text": s.Text})
	}
	return map[string]any{"sources": sources}
}

// noTests hides _test.go files, so answers come from the README and the
// engine's real code rather than from test cases.
type noTests struct{ fs.ReadDirFS }

func (n noTests) ReadDir(name string) ([]fs.DirEntry, error) {
	all, err := n.ReadDirFS.ReadDir(name)
	kept := all[:0]
	for _, e := range all {
		if !strings.HasSuffix(e.Name(), "_test.go") {
			kept = append(kept, e)
		}
	}
	return kept, err
}
