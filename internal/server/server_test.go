package server

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/client"
	"github.com/ABDULRAIYAAN/vaultdb/internal/engine"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

// start runs a server on a free local port and returns its address.
func start(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(engine.New(store.New()), opts)
	go srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return srv, ln.Addr().String()
}

func dial(t *testing.T, addr string) *client.Client {
	t.Helper()
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func do(t *testing.T, c *client.Client, cmd string) string {
	t.Helper()
	reply, err := c.Do(cmd)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return reply
}

func TestBasicCommands(t *testing.T) {
	_, addr := start(t, Options{})
	c := dial(t, addr)
	steps := []struct{ in, want string }{
		{"PING", "PONG"},
		{"SET name raiyaan", "OK"},
		{"GET name", `"raiyaan"`},
		{"CREATE TABLE t (id INT PRIMARY KEY, name TEXT)", "OK, table t created"},
		{"INSERT INTO t VALUES (1, 'Ali')", "OK, 1 row inserted"},
		// A reply with several lines arrives whole, thanks to the length header.
		{"SELECT * FROM t", "+----+-------+\n| id | name  |\n+----+-------+\n| 1  | 'Ali' |\n+----+-------+\n(1 row)"},
		{"", ""},
	}
	for _, s := range steps {
		if got := do(t, c, s.in); got != s.want {
			t.Errorf("%q\n got %q\nwant %q", s.in, got, s.want)
		}
	}
}

// Two clients see the same data.
func TestClientsShareData(t *testing.T) {
	_, addr := start(t, Options{})
	a, b := dial(t, addr), dial(t, addr)
	do(t, a, "SET shared hello")
	if got := do(t, b, "GET shared"); got != `"hello"` {
		t.Fatalf("second client got %q", got)
	}
}

// 50 clients writing at the same time: nothing lost, nothing duplicated.
func TestManyClientsAtOnce(t *testing.T) {
	_, addr := start(t, Options{})
	do(t, dial(t, addr), "CREATE TABLE hits (id INT PRIMARY KEY, who INT)")

	const clients, each = 50, 20
	var wg sync.WaitGroup
	var dupes sync.Map
	for w := 0; w < clients; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c, err := client.Dial(addr)
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			for i := 0; i < each; i++ {
				c.Do(fmt.Sprintf("SET k:%d:%d v", w, i))
				// Every client fights over the same 20 ids; exactly one INSERT
				// per id may succeed.
				r, _ := c.Do(fmt.Sprintf("INSERT INTO hits VALUES (%d, %d)", i, w))
				if r == "OK, 1 row inserted" {
					if _, loaded := dupes.LoadOrStore(i, w); loaded {
						t.Errorf("id %d inserted twice", i)
					}
				}
			}
		}(w)
	}
	wg.Wait()

	c := dial(t, addr)
	if got := do(t, c, "PREFIX k: 2000"); strings.Count(got, "\n")+1 != clients*each {
		t.Fatalf("expected %d keys, got %d lines", clients*each, strings.Count(got, "\n")+1)
	}
	if got := do(t, c, "SELECT COUNT(*) FROM hits"); !strings.Contains(got, "| 20    |") {
		t.Fatalf("hits table:\n%s", got)
	}
}

func TestQuit(t *testing.T) {
	_, addr := start(t, Options{})
	c := dial(t, addr)
	if got := do(t, c, "quit"); got != "Bye." {
		t.Fatalf("QUIT reply %q", got)
	}
	if _, err := c.Do("PING"); err == nil {
		t.Fatal("connection should be closed after QUIT")
	}
}

func TestLineTooLong(t *testing.T) {
	_, addr := start(t, Options{MaxLine: 100})
	c := dial(t, addr)
	got, err := c.Do("SET k " + strings.Repeat("x", 200))
	if err != nil || !strings.Contains(got, "longer than 100 bytes") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestIdleClientsAreDisconnected(t *testing.T) {
	_, addr := start(t, Options{IdleTimeout: 100 * time.Millisecond})
	c := dial(t, addr)
	time.Sleep(300 * time.Millisecond)
	if _, err := c.Do("PING"); err == nil {
		t.Fatal("idle client should have been disconnected")
	}
}

func TestConnectionLimit(t *testing.T) {
	_, addr := start(t, Options{MaxConnections: 1})
	first := dial(t, addr)
	do(t, first, "PING") // make sure the first one is registered

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(conn)
	r.ReadString('\n') // length header
	msg, _ := r.ReadString('\n')
	if !strings.Contains(msg, "server is full") {
		t.Fatalf("second client got %q", msg)
	}
}

// Works with plain text tools: telnet-style clients can send \r\n.
func TestRawProtocol(t *testing.T) {
	_, addr := start(t, Options{})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "SET a 1\r\nGET a\r\n")
	r := bufio.NewReader(conn)
	var got []string
	for i := 0; i < 2; i++ {
		var n int
		fmt.Fscanf(r, "%d\n", &n)
		buf := make([]byte, n)
		r.Read(buf)
		got = append(got, string(buf))
	}
	if strings.Join(got, "|") != `OK|"1"` {
		t.Fatalf("raw replies %q", got)
	}
}

func TestShutdown(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := New(engine.New(store.New()), Options{})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	c, err := client.Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	do(t, c, "PING")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-served; err != ErrServerClosed {
		t.Fatalf("Serve returned %v", err)
	}
	if _, err := c.Do("PING"); err == nil {
		t.Fatal("client should be disconnected after shutdown")
	}
	if _, err := client.Dial(ln.Addr().String()); err == nil {
		t.Fatal("new connections should be refused after shutdown")
	}
}

func TestClientRejectsMultiLine(t *testing.T) {
	_, addr := start(t, Options{})
	if _, err := dial(t, addr).Do("SET a 1\nSET b 2"); err == nil {
		t.Fatal("multi-line command should be rejected by the client")
	}
}
