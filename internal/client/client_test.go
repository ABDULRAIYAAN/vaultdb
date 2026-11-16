package client

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeServer listens on a free local port and answers each command line
// with whatever reply returns, written raw so tests can send broken replies.
func fakeServer(t *testing.T, reply func(cmd string) string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if _, err := conn.Write([]byte(reply(strings.TrimSuffix(line, "\n")))); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// frame encodes a reply the way a real server does: length, newline, body.
func frame(body string) string { return fmt.Sprintf("%d\n%s", len(body), body) }

func dialT(t *testing.T, addr string) *Client {
	t.Helper()
	c, err := Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestDoReadsFramedReplies(t *testing.T) {
	addr := fakeServer(t, func(cmd string) string {
		switch cmd {
		case "PING":
			return frame("PONG")
		case "TABLE":
			return frame("+---+\n| a |\n+---+") // newlines inside the body
		case "EMPTY":
			return frame("")
		}
		return frame("echo " + cmd)
	})
	c := dialT(t, addr)
	for _, s := range []struct{ in, want string }{
		{"PING", "PONG"},
		{"TABLE", "+---+\n| a |\n+---+"},
		{"EMPTY", ""},
		{"GET name", "echo GET name"},
	} {
		got, err := c.Do(s.in)
		if err != nil {
			t.Fatalf("%s: %v", s.in, err)
		}
		if got != s.want {
			t.Errorf("%s: got %q, want %q", s.in, got, s.want)
		}
	}
}

func TestDoRejectsMultiLineCommands(t *testing.T) {
	c := dialT(t, fakeServer(t, func(string) string { return frame("OK") }))
	for _, cmd := range []string{"SET a 1\nDEL a", "GET a\r"} {
		if _, err := c.Do(cmd); err == nil {
			t.Errorf("%q: expected an error", cmd)
		}
	}
	// The connection is still usable afterwards.
	if got, err := c.Do("PING"); err != nil || got != "OK" {
		t.Errorf("after rejected command: got %q, %v", got, err)
	}
}

func TestDoRejectsBadHeaders(t *testing.T) {
	for _, raw := range []string{
		"abc\nxyz",                      // not a number
		"-1\n",                          // negative
		fmt.Sprintf("%d\n", MaxReply+1), // too large to accept
		"",                              // server hangs up without replying
		"10\nshort",                     // body cut off before 10 bytes
	} {
		c := dialT(t, fakeServerOnce(t, raw))
		if got, err := c.Do("GET x"); err == nil {
			t.Errorf("reply %q: expected an error, got %q", raw, got)
		}
	}
}

// fakeServerOnce writes raw after the first command and closes the
// connection, for replies that are cut off.
func fakeServerOnce(t *testing.T, raw string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
			return
		}
		conn.Write([]byte(raw))
	}()
	return ln.Addr().String()
}

func TestDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now
	if _, err := Dial(addr); err == nil {
		t.Error("expected Dial to fail when nothing is listening")
	}
}

func TestDoAfterClose(t *testing.T) {
	c := dialT(t, fakeServer(t, func(string) string { return frame("OK") }))
	c.Close()
	if _, err := c.Do("PING"); err == nil {
		t.Error("expected an error on a closed client")
	}
}

// TestSharedClient sends commands from many goroutines through one client.
// The mutex must keep each command paired with its own reply.
func TestSharedClient(t *testing.T) {
	c := dialT(t, fakeServer(t, func(cmd string) string { return frame("re:" + cmd) }))
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				cmd := fmt.Sprintf("GET k%d_%d", g, i)
				got, err := c.Do(cmd)
				if err != nil {
					errs <- err
					return
				}
				if got != "re:"+cmd {
					errs <- fmt.Errorf("%s got reply %q", cmd, got)
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
