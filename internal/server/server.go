// Package server lets other programs talk to VaultDB over the network.
//
// It listens on a TCP port. Each client that connects gets its own goroutine
// (a lightweight thread), so hundreds of clients can be served at once while
// the store's lock keeps their reads and writes safe.
//
// The protocol is deliberately simple, so it can be used from any language
// or even by hand with a tool like telnet:
//
//	client -> server   one command per line, ending in \n
//	                   SET name raiyaan\n
//
//	server -> client   the length of the reply in bytes, a newline,
//	                   then exactly that many bytes
//	                   2\nOK
//
// Sending the length first means a reply can contain newlines (like a SQL
// result table) and the client still knows exactly where it ends. Redis's
// protocol uses the same idea for its "bulk strings".
package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/engine"
)

// Defaults for Options.
const (
	DefaultMaxLine        = 1 << 20 // 1 MB per command
	DefaultIdleTimeout    = 5 * time.Minute
	DefaultMaxConnections = 1000
)

// Options tunes the server.
type Options struct {
	MaxLine        int           // longest command accepted, in bytes
	IdleTimeout    time.Duration // disconnect clients silent for this long
	MaxConnections int           // refuse new clients beyond this many
	Logger         *log.Logger   // nil means no logging
}

// Server serves one engine to many network clients.
type Server struct {
	eng  *engine.Engine
	opts Options

	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]struct{}
	closing  bool
	handlers sync.WaitGroup
}

// New creates a server for eng.
func New(eng *engine.Engine, opts Options) *Server {
	if opts.MaxLine <= 0 {
		opts.MaxLine = DefaultMaxLine
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.MaxConnections <= 0 {
		opts.MaxConnections = DefaultMaxConnections
	}
	return &Server{eng: eng, opts: opts, conns: map[net.Conn]struct{}{}}
}

// ErrServerClosed is returned by Serve after Shutdown.
var ErrServerClosed = errors.New("server closed")

func (s *Server) logf(format string, args ...any) {
	if s.opts.Logger != nil {
		s.opts.Logger.Printf(format, args...)
	}
}

// Serve accepts clients on ln until Shutdown is called.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return ErrServerClosed
	}
	s.ln = ln
	s.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond) // e.g. too many open files: back off and retry
				continue
			}
			return err
		}

		s.mu.Lock()
		if s.closing || len(s.conns) >= s.opts.MaxConnections {
			full := !s.closing
			s.mu.Unlock()
			if full {
				writeReply(conn, "(error) server is full, try again later")
			}
			conn.Close()
			continue
		}
		s.conns[conn] = struct{}{}
		s.handlers.Add(1)
		s.mu.Unlock()

		go s.handle(conn)
	}
}

// handle serves one client until it disconnects, goes quiet, or the server
// shuts down.
func (s *Server) handle(conn net.Conn) {
	defer func() {
		s.logf("client disconnected: %s", conn.RemoteAddr())
		conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		s.handlers.Done()
	}()
	s.logf("client connected: %s", conn.RemoteAddr())

	r := bufio.NewReaderSize(conn, 64*1024)
	for {
		// Checked every loop: a client that was mid-command when Shutdown
		// began must not start waiting for another command.
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing {
			return
		}
		conn.SetReadDeadline(time.Now().Add(s.opts.IdleTimeout))
		line, err := readLine(r, s.opts.MaxLine)
		if errors.Is(err, errLineTooLong) {
			writeReply(conn, fmt.Sprintf("(error) command longer than %d bytes", s.opts.MaxLine))
			return // the rest of the line is still unread, so the stream is out of step
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !isClosedOrTimeout(err) {
				s.logf("read from %s: %v", conn.RemoteAddr(), err)
			}
			return
		}

		var reply string
		switch upper := strings.ToUpper(strings.TrimSpace(line)); upper {
		case "PING":
			reply = "PONG"
		case "QUIT", "EXIT":
			writeReply(conn, "Bye.")
			return
		default:
			reply = s.eng.ExecuteLine(line)
		}

		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if err := writeReply(conn, reply); err != nil {
			return
		}
	}
}

var errLineTooLong = errors.New("line too long")

// readLine reads up to and including '\n' and returns the line without it.
// It never holds more than max bytes, so a client can't exhaust memory by
// sending one endless line.
func readLine(r *bufio.Reader, max int) (string, error) {
	var b strings.Builder
	for {
		chunk, err := r.ReadSlice('\n')
		if b.Len()+len(chunk) > max+1 { // +1 for the newline itself
			return "", errLineTooLong
		}
		b.Write(chunk)
		switch {
		case err == nil:
			return strings.TrimRight(b.String(), "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && b.Len() > 0:
			return strings.TrimRight(b.String(), "\r\n"), nil // last line without \n
		default:
			return "", err
		}
	}
}

func writeReply(w io.Writer, reply string) error {
	_, err := fmt.Fprintf(w, "%d\n%s", len(reply), reply)
	return err
}

func isClosedOrTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, net.ErrClosed) || (errors.As(err, &ne) && ne.Timeout())
}

// Addr returns the address the server listens on (useful with port 0).
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Shutdown stops accepting clients, lets each client finish the command it
// is running, then disconnects everyone. It returns when all clients are
// gone or ctx expires.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	if s.ln != nil {
		s.ln.Close()
	}
	// Waking blocked reads makes idle clients exit at once. A client in the
	// middle of a command finishes it first, because the engine call doesn't
	// read from the connection.
	for c := range s.conns {
		c.SetReadDeadline(time.Now())
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		return ctx.Err()
	}
}
