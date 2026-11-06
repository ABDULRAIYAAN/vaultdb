// Command vaultdb-web runs the VaultDB playground website.
//
//	go run ./cmd/vaultdb-web                    http://localhost:8080
//	go run ./cmd/vaultdb-web -addr :80 -https   behind your HTTPS proxy
//
// The docs assistant (/api/ask) reads the folder given by -docs at startup.
// It uses the same provider settings as vaultdb-chat (VAULTDB_EMBED,
// VAULTDB_LLM, API keys), from the environment or vaultdb-chat.env.
package main

import (
	"bufio"
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ABDULRAIYAAN/vaultdb/internal/ai"
	"github.com/ABDULRAIYAAN/vaultdb/internal/rag"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
	"github.com/ABDULRAIYAAN/vaultdb/internal/web"
	webui "github.com/ABDULRAIYAAN/vaultdb/web"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "address to listen on")
	docs := flag.String("docs", ".", "folder the docs assistant answers from (\"\" to turn it off)")
	https := flag.Bool("https", false, "the site is served over HTTPS (secure cookies, HSTS)")
	trustProxy := flag.Bool("trust-proxy", false, "read visitor IPs from X-Forwarded-For (only behind your own proxy)")
	askPerDay := flag.Int("ask-per-day", 500, "questions the docs assistant answers per day, to cap AI costs")
	flag.Parse()
	loadEnvFile("vaultdb-chat.env")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := web.Options{
		Static:        webui.Files(),
		TrustProxy:    *trustProxy,
		SecureCookies: *https,
		AskPerDay:     *askPerDay,
	}
	if *docs != "" {
		r, err := docsAssistant(ctx, *docs)
		if err != nil {
			log.Printf("docs assistant off: %v", err)
		} else {
			opts.RAG = r
		}
	}

	srv := web.New(opts)
	log.Printf("VaultDB playground on http://%s  (Ctrl+C to stop)", *addr)
	if err := srv.Run(ctx, *addr); err != nil && !strings.Contains(err.Error(), "Server closed") {
		log.Fatal(err)
	}
	log.Print("stopped")
}

// docsAssistant reads the docs folder into an in-memory VaultDB.
func docsAssistant(ctx context.Context, folder string) (*rag.RAG, error) {
	emb, err := ai.NewEmbedder(ai.Config{Provider: envOr("VAULTDB_EMBED", "local"), Model: os.Getenv("VAULTDB_EMBED_MODEL")})
	if err != nil {
		return nil, err
	}
	chat, err := ai.NewChatModel(ai.Config{Provider: envOr("VAULTDB_LLM", "none"), Model: os.Getenv("VAULTDB_MODEL")})
	if err != nil {
		return nil, err
	}
	r := rag.New(store.New(), emb, chat, "docs")
	rep, err := r.Ingest(ctx, folder, nil)
	if err != nil {
		return nil, err
	}
	answers := "passages only"
	if chat != nil {
		answers = chat.Name()
	}
	log.Printf("docs assistant: %d files, %d chunks (embeddings: %s, answers: %s)", rep.Added, rep.Chunks, emb.Name(), answers)
	return r, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
			if k != "" && os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
	}
}
