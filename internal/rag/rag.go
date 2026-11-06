// Package rag answers questions about your own documents using VaultDB as
// the memory. RAG stands for Retrieval-Augmented Generation:
//
//	INGEST (once)                          ASK (every question)
//	folder of .md / .txt files             "What does the report say about X?"
//	        |                                       |
//	   split into chunks                       embed the question
//	        |                                       |
//	   embed each chunk  ---> VaultDB <---  VSEARCH: nearest chunks
//	                       (HNSW index)             |
//	                                        question + best chunks -> chat model
//	                                                |
//	                                        answer that cites [1], [2] ...
//
// The model only sees the few chunks VaultDB found, so it answers from your
// documents instead of from memory, and every claim can be traced to a file.
package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ABDULRAIYAAN/vaultdb/internal/ai"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
	"github.com/ABDULRAIYAAN/vaultdb/internal/vector"
)

// Keys used besides the vector collection itself:
//
//	rag:meta:<coll>          which embedding model built the collection
//	rag:file:<coll>:<path>   content hash and chunk count of each ingested file
const (
	metaPrefix = "rag:meta:"
	filePrefix = "rag:file:"
)

// Extensions that are read as plain text.
var TextExtensions = map[string]bool{
	".md": true, ".txt": true, ".markdown": true, ".rst": true, ".csv": true,
	".go": true, ".py": true, ".js": true, ".ts": true, ".java": true, ".html": true, ".json": true,
}

// Chunk sizes, in characters. Overlap repeats the end of one chunk at the
// start of the next, so a sentence cut at a boundary is still whole in one.
const (
	ChunkSize    = 1000
	ChunkOverlap = 150
	embedBatch   = 32
)

// RAG ties a store, a vector collection and the two models together.
type RAG struct {
	st         *store.Store
	vec        *vector.Manager
	embed      ai.Embedder
	chat       ai.ChatModel // nil: show passages only
	collection string
}

// New creates a RAG over st using the given models. chat may be nil.
func New(st *store.Store, embed ai.Embedder, chat ai.ChatModel, collection string) *RAG {
	return &RAG{st: st, vec: vector.NewManager(st), embed: embed, chat: chat, collection: collection}
}

type collMeta struct {
	Embedder string `json:"embedder"`
	Dim      int    `json:"dim"`
}

type fileMeta struct {
	Hash   string `json:"hash"`
	Chunks int    `json:"chunks"`
}

// Chunk is one stored passage.
type Chunk struct {
	Source string `json:"src"`
	Text   string `json:"text"`
}

// IngestReport says what Ingest did.
type IngestReport struct {
	Added, Updated, Unchanged, Removed, Skipped int
	Chunks                                      int
}

// Ingest reads every text file under root and stores its chunks. Files that
// haven't changed since the last run are skipped, changed files have their
// old chunks replaced, so running it again is cheap.
func (r *RAG) Ingest(ctx context.Context, root string, progress func(string)) (IngestReport, error) {
	var rep IngestReport
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if TextExtensions[strings.ToLower(filepath.Ext(path))] {
			files = append(files, path)
		} else {
			rep.Skipped++
		}
		return nil
	})
	if err != nil {
		return rep, err
	}
	if len(files) == 0 {
		return rep, fmt.Errorf("no text files found in %s (supported: .md .txt and common code files; convert PDFs and Word files to text first)", root)
	}
	sort.Strings(files)
	seen := map[string]bool{}

	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return rep, err
		}
		if !utf8.Valid(data) {
			rep.Skipped++
			continue
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])

		old, existed := r.fileMeta(rel)
		if existed && old.Hash == hash {
			rep.Unchanged++
			continue
		}
		chunks := Split(string(data), ChunkSize, ChunkOverlap)
		if progress != nil {
			progress(fmt.Sprintf("%s (%d chunks)", rel, len(chunks)))
		}
		if err := r.storeChunks(ctx, rel, chunks); err != nil {
			return rep, fmt.Errorf("%s: %w", rel, err)
		}
		// Remove leftover chunks if the file got shorter.
		for i := len(chunks); existed && i < old.Chunks; i++ {
			r.vec.Delete(r.collection, chunkID(rel, i))
		}
		raw, _ := json.Marshal(fileMeta{Hash: hash, Chunks: len(chunks)})
		if err := r.st.Set(filePrefix+r.collection+":"+rel, string(raw)); err != nil {
			return rep, err
		}
		rep.Chunks += len(chunks)
		if existed {
			rep.Updated++
		} else {
			rep.Added++
		}
	}

	// Files ingested before but gone from the folder now: forget them.
	known, _ := r.st.Prefix(filePrefix+r.collection+":", 0)
	for _, kv := range known {
		rel := strings.TrimPrefix(kv.Key, filePrefix+r.collection+":")
		if seen[rel] {
			continue
		}
		var m fileMeta
		json.Unmarshal([]byte(kv.Value), &m)
		for i := 0; i < m.Chunks; i++ {
			r.vec.Delete(r.collection, chunkID(rel, i))
		}
		r.st.Delete(kv.Key)
		rep.Removed++
	}
	return rep, nil
}

func chunkID(path string, i int) string { return path + "#" + strconv.Itoa(i) }

func (r *RAG) fileMeta(rel string) (fileMeta, bool) {
	raw, err := r.st.Get(filePrefix + r.collection + ":" + rel)
	if err != nil {
		return fileMeta{}, false
	}
	var m fileMeta
	return m, json.Unmarshal([]byte(raw), &m) == nil
}

// storeChunks embeds chunks in batches and adds them to the collection,
// creating it (sized to the model's vectors) on first use.
func (r *RAG) storeChunks(ctx context.Context, rel string, chunks []string) error {
	for start := 0; start < len(chunks); start += embedBatch {
		end := min(start+embedBatch, len(chunks))
		vecs, err := r.embed.Embed(ctx, chunks[start:end])
		if err != nil {
			return err
		}
		if err := r.ensureCollection(len(vecs[0])); err != nil {
			return err
		}
		for i, v := range vecs {
			payload, _ := json.Marshal(Chunk{Source: rel, Text: chunks[start+i]})
			if _, err := r.vec.Add(r.collection, chunkID(rel, start+i), v, string(payload)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureCollection creates the collection the first time, and afterwards
// refuses to mix in vectors from a different embedding model: vectors from
// two models live in unrelated spaces, so comparing them is meaningless.
func (r *RAG) ensureCollection(dim int) error {
	raw, err := r.st.Get(metaPrefix + r.collection)
	if err == nil {
		var m collMeta
		json.Unmarshal([]byte(raw), &m)
		if m.Embedder != r.embed.Name() {
			return fmt.Errorf("collection %s was built with %s, but you are using %s; use the same embedding model, or a different -collection",
				r.collection, m.Embedder, r.embed.Name())
		}
		return nil
	}
	if err := r.vec.Create(r.collection, dim, vector.Cosine, vector.IndexHNSW); err != nil {
		return err
	}
	meta, _ := json.Marshal(collMeta{Embedder: r.embed.Name(), Dim: dim})
	return r.st.Set(metaPrefix+r.collection, string(meta))
}

// Source is one retrieved passage, numbered as the answer cites it.
type Source struct {
	N        int
	File     string
	Text     string
	Distance float32
}

// Answer is the reply to a question.
type Answer struct {
	Text    string // empty when there is no chat model
	Sources []Source
}

// SystemPrompt tells the chat model how to use the passages.
const SystemPrompt = `You answer questions using ONLY the numbered sources provided with each question.
Cite the sources you use like [1] or [2][3] right after the sentence they support.
If the sources do not contain the answer, say you could not find it in the documents. Do not guess.
Be clear and concise.`

// Ask finds the k most relevant chunks for question and, if a chat model is
// set, has it write an answer from them. history holds earlier turns of the
// conversation (without their sources) so follow-up questions make sense.
func (r *RAG) Ask(ctx context.Context, question string, k int, history []ai.Message) (Answer, error) {
	raw, err := r.st.Get(metaPrefix + r.collection)
	if errors.Is(err, store.ErrNotFound) {
		return Answer{}, fmt.Errorf("collection %s is empty: run 'vaultdb-chat ingest <folder>' first", r.collection)
	}
	var m collMeta
	json.Unmarshal([]byte(raw), &m)
	if m.Embedder != r.embed.Name() {
		return Answer{}, fmt.Errorf("collection %s was built with %s; ask with the same embedding model (now %s)", r.collection, m.Embedder, r.embed.Name())
	}

	qv, err := r.embed.Embed(ctx, []string{question})
	if err != nil {
		return Answer{}, err
	}
	hits, err := r.vec.Search(r.collection, qv[0], k)
	if err != nil {
		return Answer{}, err
	}
	var ans Answer
	var ctxText strings.Builder
	for i, h := range hits {
		var c Chunk
		json.Unmarshal([]byte(h.Payload), &c)
		ans.Sources = append(ans.Sources, Source{N: i + 1, File: c.Source, Text: c.Text, Distance: h.Distance})
		fmt.Fprintf(&ctxText, "[%d] (from %s)\n%s\n\n", i+1, c.Source, c.Text)
	}
	if r.chat == nil {
		return ans, nil
	}
	if len(hits) == 0 {
		ans.Text = "The collection has no documents yet."
		return ans, nil
	}
	msgs := append(append([]ai.Message(nil), history...), ai.Message{
		Role:    "user",
		Content: "Sources:\n\n" + ctxText.String() + "Question: " + question,
	})
	ans.Text, err = r.chat.Chat(ctx, SystemPrompt, msgs)
	return ans, err
}

// Split cuts text into chunks of about size characters, preferring to break
// at paragraph ends, then sentence ends, then spaces, and repeating overlap
// characters between neighbouring chunks.
func Split(text string, size, overlap int) []string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= size {
		return []string{text}
	}
	var chunks []string
	start := 0
	for start < len(runes) {
		end := min(start+size, len(runes))
		if end < len(runes) {
			end = bestBreak(runes, start+size/2, end)
		}
		if c := strings.TrimSpace(string(runes[start:end])); c != "" {
			chunks = append(chunks, c)
		}
		if end >= len(runes) {
			break
		}
		next := end - overlap
		if next <= start {
			next = end
		}
		// Start the overlap at a word boundary.
		for next < end && next > start && runes[next-1] != ' ' && runes[next-1] != '\n' {
			next++
		}
		start = next
	}
	return chunks
}

// bestBreak finds the best place to end a chunk between lo and hi.
func bestBreak(r []rune, lo, hi int) int {
	for _, sep := range []string{"\n\n", ". ", "\n", " "} {
		s := []rune(sep)
		for i := hi; i >= lo+len(s); i-- {
			if string(r[i-len(s):i]) == sep {
				return i
			}
		}
	}
	return hi
}
