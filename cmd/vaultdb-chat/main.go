// Command vaultdb-chat lets you chat with your own documents, using VaultDB
// as the memory (RAG: retrieval-augmented generation).
//
//	vaultdb-chat providers                       list every supported AI provider
//	vaultdb-chat ingest <folder> [flags]         read a folder into VaultDB
//	vaultdb-chat ask [flags] "question"          one question, one answer
//	vaultdb-chat chat [flags]                    interactive conversation
//
// Choose the models with flags, for example:
//
//	-embed ollama -llm ollama                    everything free on your laptop
//	-embed gemini -llm gemini                    Google (free tier, needs GEMINI_API_KEY)
//	-embed ollama -llm groq                      local embeddings, fast cloud answers
//	-embed openai -llm anthropic -model claude-sonnet-5-5
//
// API keys come from environment variables (see 'providers'), or from a file
// named vaultdb-chat.env next to where you run it, with lines like
// GROQ_API_KEY=... (that file is git-ignored). The same file can set your
// usual choices so you don't have to type flags:
//
//	VAULTDB_EMBED=ollama
//	VAULTDB_LLM=groq
//	VAULTDB_MODEL=llama-3.3-70b-versatile
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/ABDULRAIYAAN/vaultdb/internal/ai"
	"github.com/ABDULRAIYAAN/vaultdb/internal/rag"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

type options struct {
	data, collection            string
	embed, embedModel, embedURL string
	llm, model, llmURL          string
	k                           int
	showText                    bool
}

func main() {
	loadEnvFile("vaultdb-chat.env")
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "providers" {
		listProviders()
		return
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var o options
	fs.StringVar(&o.data, "data", filepath.Join("data", "vaultdb.log"), "VaultDB data file")
	fs.StringVar(&o.collection, "collection", "docs", "vector collection to use")
	fs.StringVar(&o.embed, "embed", envOr("VAULTDB_EMBED", "local"), "embedding provider (see 'providers')")
	fs.StringVar(&o.embedModel, "embed-model", os.Getenv("VAULTDB_EMBED_MODEL"), "embedding model (default: the provider's)")
	fs.StringVar(&o.embedURL, "embed-url", "", "override the embedding provider's base URL")
	fs.StringVar(&o.llm, "llm", envOr("VAULTDB_LLM", "none"), "chat provider for answers (see 'providers'); none = show passages only")
	fs.StringVar(&o.model, "model", os.Getenv("VAULTDB_MODEL"), "chat model (default: the provider's)")
	fs.StringVar(&o.llmURL, "llm-url", "", "override the chat provider's base URL")
	fs.IntVar(&o.k, "k", 4, "how many passages to retrieve per question")
	fs.BoolVar(&o.showText, "show-text", false, "print the retrieved passages, not just their file names")

	// Allow flags before or after the positional argument.
	var positional []string
	for len(args) > 0 {
		fs.Parse(args)
		args = fs.Args()
		if len(args) > 0 {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch cmd {
	case "ingest":
		if len(positional) != 1 {
			fail("usage: vaultdb-chat ingest <folder> [flags]")
		}
		err = ingest(ctx, o, positional[0])
	case "ask":
		if len(positional) == 0 {
			fail(`usage: vaultdb-chat ask [flags] "your question"`)
		}
		err = ask(ctx, o, strings.Join(positional, " "))
	case "chat":
		err = chat(ctx, o)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fail("error: " + err.Error())
	}
}

func usage() {
	fmt.Print(`vaultdb-chat: chat with your documents, using VaultDB as the memory

  vaultdb-chat providers                    list AI providers and their default models
  vaultdb-chat ingest <folder> [flags]      read .md/.txt/code files into VaultDB
  vaultdb-chat ask [flags] "question"       ask one question
  vaultdb-chat chat [flags]                 interactive conversation

Flags:
  -embed <provider>      embeddings (default local: built in, no setup)
  -embed-model <name>    override the embedding model
  -llm <provider>        writes the answers (default none: show passages only)
  -model <name>          override the chat model
  -k <n>                 passages per question (default 4)
  -collection <name>     default docs
  -data <file>           default data/vaultdb.log
  -show-text             print retrieved passages in full

Examples:
  vaultdb-chat ingest ./notes -embed ollama
  vaultdb-chat chat -embed ollama -llm ollama
  vaultdb-chat ask -embed ollama -llm groq "What are the main risks?"

Use the same -embed for ingest and for questions. Stop any running
'vaultdb -serve' that uses the same data file first.
`)
}

func listProviders() {
	fmt.Printf("%-11s %-24s %-26s %-20s %s\n", "PROVIDER", "CHAT MODEL", "EMBEDDING MODEL", "API KEY", "NOTE")
	for _, name := range ai.ProviderNames() {
		p := ai.Providers[name]
		key := p.KeyEnv
		if key == "" {
			key = "(none needed)"
		} else if os.Getenv(key) != "" {
			key += " ✓"
		}
		fmt.Printf("%-11s %-24s %-26s %-20s %s\n", name, orDash(p.ChatModel), orDash(p.EmbedModel), key, p.Note)
	}
	fmt.Println("\nAny model a provider offers can be chosen with -model / -embed-model.")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func open(o options, needChat bool) (*rag.RAG, *store.Store, error) {
	emb, err := ai.NewEmbedder(ai.Config{Provider: o.embed, Model: o.embedModel, BaseURL: o.embedURL})
	if err != nil {
		return nil, nil, err
	}
	var chat ai.ChatModel
	if needChat {
		if chat, err = ai.NewChatModel(ai.Config{Provider: o.llm, Model: o.model, BaseURL: o.llmURL}); err != nil {
			return nil, nil, err
		}
	}
	st, _, err := store.Open(o.data, store.DefaultOptions())
	if err != nil {
		return nil, nil, err
	}
	return rag.New(st, emb, chat, o.collection), st, nil
}

func ingest(ctx context.Context, o options, folder string) error {
	r, st, err := open(o, false)
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Printf("Ingesting %s into collection %q...\n", folder, o.collection)
	rep, err := r.Ingest(ctx, folder, func(s string) { fmt.Println("  +", s) })
	if err != nil {
		return err
	}
	fmt.Printf("Done: %d new, %d updated, %d unchanged, %d removed files (%d chunks embedded, %d files skipped).\n",
		rep.Added, rep.Updated, rep.Unchanged, rep.Removed, rep.Chunks, rep.Skipped)
	return nil
}

func ask(ctx context.Context, o options, q string) error {
	r, st, err := open(o, true)
	if err != nil {
		return err
	}
	defer st.Close()
	ans, err := r.Ask(ctx, q, o.k, nil)
	if err != nil {
		return err
	}
	printAnswer(ans, o.showText || ans.Text == "")
	return nil
}

func chat(ctx context.Context, o options) error {
	r, st, err := open(o, true)
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Printf("Chatting with collection %q (embed: %s, answers: %s). Commands: /text /clear /exit\n", o.collection, o.embed, o.llm)
	var history []ai.Message
	show := o.showText
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\nyou> ")
		if !sc.Scan() || ctx.Err() != nil {
			fmt.Println()
			return nil
		}
		q := strings.TrimSpace(sc.Text())
		switch q {
		case "":
			continue
		case "/exit", "/quit", "exit", "quit":
			return nil
		case "/clear":
			history = nil
			fmt.Println("(conversation cleared)")
			continue
		case "/text":
			show = !show
			fmt.Printf("(show passages: %v)\n", show)
			continue
		}
		ans, err := r.Ask(ctx, q, o.k, history)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		printAnswer(ans, show || ans.Text == "")
		if ans.Text != "" {
			history = append(history, ai.Message{Role: "user", Content: q}, ai.Message{Role: "assistant", Content: ans.Text})
			if len(history) > 8 { // keep the last 4 exchanges
				history = history[len(history)-8:]
			}
		}
	}
}

func printAnswer(a rag.Answer, showText bool) {
	if a.Text != "" {
		fmt.Println("\n" + a.Text)
	}
	if len(a.Sources) == 0 {
		fmt.Println("(no matching passages)")
		return
	}
	fmt.Println("\nSources:")
	for _, s := range a.Sources {
		fmt.Printf("  [%d] %s  (distance %.3f)\n", s.N, s.File, s.Distance)
		if showText {
			for _, line := range strings.Split(s.Text, "\n") {
				fmt.Println("      " + line)
			}
		}
	}
}

// loadEnvFile sets KEY=VALUE lines from path as environment variables,
// without overriding variables that are already set.
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
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if ok && k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
