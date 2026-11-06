package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/ai"
	"github.com/ABDULRAIYAAN/vaultdb/internal/rag"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

func newSite(t *testing.T, opts Options) (*Server, *httptest.Server) {
	t.Helper()
	if opts.Static == nil {
		opts.Static = fstest.MapFS{
			"index.html":    {Data: []byte("<h1>VaultDB</h1>")},
			"privacy.html":  {Data: []byte("privacy")},
			"assets/app.js": {Data: []byte("console.log(1)")},
		}
	}
	s := New(opts)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

// visitor is a browser with its own cookie jar.
func visitor(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func post(t *testing.T, c *http.Client, url, body string, out any) int {
	t.Helper()
	resp, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(out)
	return resp.StatusCode
}

func exec(t *testing.T, c *http.Client, base, cmd string) execReply {
	t.Helper()
	var r execReply
	raw, _ := json.Marshal(execRequest{Command: cmd})
	post(t, c, base+"/api/exec", string(raw), &r)
	return r
}

func TestExecAndSandboxes(t *testing.T) {
	_, ts := newSite(t, Options{})
	alice, bob := visitor(t), visitor(t)

	if r := exec(t, alice, ts.URL, "SET name alice"); r.Reply != "OK" || r.Keys != 1 || r.Bytes == 0 {
		t.Fatalf("alice SET: %+v", r)
	}
	if r := exec(t, alice, ts.URL, "GET name"); r.Reply != `"alice"` {
		t.Fatalf("alice GET: %+v", r)
	}
	// Bob has his own sandbox and cannot see Alice's data.
	if r := exec(t, bob, ts.URL, "GET name"); r.Reply != "(nil)" || r.Keys != 0 {
		t.Fatalf("bob sees alice's data: %+v", r)
	}
	// SQL and vectors work too.
	exec(t, bob, ts.URL, "CREATE TABLE t (id INT PRIMARY KEY, v TEXT)")
	exec(t, bob, ts.URL, "INSERT INTO t VALUES (1, 'x')")
	if r := exec(t, bob, ts.URL, "SELECT COUNT(*) FROM t"); !strings.Contains(r.Reply, "| 1     |") {
		t.Fatalf("bob SQL: %+v", r)
	}
	// Reset empties only your own sandbox.
	var rr execReply
	post(t, alice, ts.URL+"/api/reset", "", &rr)
	if r := exec(t, alice, ts.URL, "KEYS"); r.Reply != "(empty)" {
		t.Fatalf("after reset: %+v", r)
	}
	if r := exec(t, bob, ts.URL, "SELECT COUNT(*) FROM t"); !strings.Contains(r.Reply, "| 1     |") {
		t.Fatal("bob's data was affected by alice's reset")
	}
}

func TestBadRequests(t *testing.T) {
	_, ts := newSite(t, Options{})
	c := visitor(t)
	cases := map[string]string{
		`not json`:                        "send JSON",
		`{"command": "   "}`:              "empty",
		`{"command": "SET a 1\nSET b 2"}`: "one command at a time",
		`{"command": "` + strings.Repeat("x", 5000) + `"}`: "longer than 4 KB",
	}
	for body, want := range cases {
		var r execReply
		if code := post(t, c, ts.URL+"/api/exec", body, &r); code != http.StatusBadRequest || !strings.Contains(r.Error, want) {
			t.Errorf("%.30q: %d %+v", body, code, r)
		}
	}
	// Bodies over 16 KB are cut off by the server.
	var r execReply
	if code := post(t, c, ts.URL+"/api/exec", `{"command":"`+strings.Repeat("y", 20000)+`"}`, &r); code != http.StatusBadRequest {
		t.Fatalf("huge body: %d", code)
	}
}

func TestSandboxSizeLimit(t *testing.T) {
	_, ts := newSite(t, Options{MaxDataBytes: 2000})
	c := visitor(t)
	val := strings.Repeat("v", 900)
	exec(t, c, ts.URL, "SET a "+val)
	exec(t, c, ts.URL, "SET b "+val)
	r := exec(t, c, ts.URL, "SET c "+val)
	if !strings.Contains(r.Reply, "limit, so it was reset") || r.Keys != 0 {
		t.Fatalf("limit not enforced: %+v", r)
	}
}

func TestRateLimit(t *testing.T) {
	_, ts := newSite(t, Options{})
	c := visitor(t)
	limited := false
	for i := 0; i < 60; i++ {
		var r execReply
		if code := post(t, c, ts.URL+"/api/exec", `{"command":"PING"}`, &r); code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("60 instant requests were never rate limited")
	}
}

func TestLimiterRefills(t *testing.T) {
	l := newLimiter(1, 2)
	now := time.Now()
	if !l.allow("ip", now) || !l.allow("ip", now) || l.allow("ip", now) {
		t.Fatal("burst of 2 not enforced")
	}
	if !l.allow("ip", now.Add(1100*time.Millisecond)) {
		t.Fatal("token not refilled after a second")
	}
	l.sweep(now.Add(time.Hour))
	if len(l.buckets) != 0 {
		t.Fatal("sweep did not forget old buckets")
	}
}

func TestSecurityHeadersAndStatic(t *testing.T) {
	_, ts := newSite(t, Options{})
	for path, want := range map[string]string{"/": "<h1>VaultDB</h1>", "/privacy": "privacy"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 100)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		if !strings.Contains(string(buf[:n]), want) {
			t.Errorf("%s served %q", path, buf[:n])
		}
		for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
			if resp.Header.Get(h) == "" {
				t.Errorf("%s: missing %s", path, h)
			}
		}
	}
	resp, _ := http.Get(ts.URL + "/assets/app.js")
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Error("assets should be cached long-term")
	}
	resp, _ = http.Get(ts.URL + "/../../etc/passwd")
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("path traversal served a file")
	}
}

func TestCookieIsLockedDown(t *testing.T) {
	_, ts := newSite(t, Options{SecureCookies: true})
	resp, _ := http.Post(ts.URL+"/api/exec", "application/json", strings.NewReader(`{"command":"PING"}`))
	resp.Body.Close()
	c := resp.Cookies()[0]
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || len(c.Value) != 32 {
		t.Fatalf("cookie %+v", c)
	}
	if resp.Header.Get("Strict-Transport-Security") == "" {
		t.Fatal("HSTS missing on HTTPS site")
	}
}

func TestIdleSandboxesAreSwept(t *testing.T) {
	s, ts := newSite(t, Options{SessionTTL: time.Minute})
	exec(t, visitor(t), ts.URL, "SET a 1")
	s.sweep(time.Now().Add(2 * time.Minute))
	if len(s.sessions) != 0 {
		t.Fatal("idle sandbox not deleted")
	}
}

func TestAskAndDailyCap(t *testing.T) {
	docs := t.TempDir()
	os.WriteFile(filepath.Join(docs, "a.md"), []byte("Compaction writes a temp file then renames it atomically."), 0o644)
	r := rag.New(store.New(), ai.NewHashEmbedder(128), nil, "docs")
	if _, err := r.Ingest(context.Background(), docs, nil); err != nil {
		t.Fatal(err)
	}
	_, ts := newSite(t, Options{RAG: r, AskPerDay: 1})
	var a askReply
	if code := post(t, visitor(t), ts.URL+"/api/ask", `{"question":"how does compaction rename"}`, &a); code != 200 || len(a.Sources) != 1 || a.Sources[0].File != "a.md" {
		t.Fatalf("ask: %d %+v", code, a)
	}
	if code := post(t, visitor(t), ts.URL+"/api/ask", `{"question":"again"}`, &a); code != http.StatusTooManyRequests || !strings.Contains(a.Error, "daily limit") {
		t.Fatalf("daily cap: %d %+v", code, a)
	}
	var st statsReply
	resp, _ := http.Get(ts.URL + "/api/stats")
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if !st.AskEnabled || st.Sandboxes < 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestAskDisabled(t *testing.T) {
	_, ts := newSite(t, Options{})
	var a askReply
	if code := post(t, visitor(t), ts.URL+"/api/ask", `{"question":"x"}`, &a); code != http.StatusNotFound {
		t.Fatalf("code %d", code)
	}
}
