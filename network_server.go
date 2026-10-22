// Package honeycomb provides a thread-safe, in-memory database for managing
// PLC-like tags. This file, network_server.go, provides the public-facing
// functions to run a network server that exposes a TagDatabase instance
// over HTTPS.
package honeycomb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Access is what a request does to the database.
type Access int

const (
	// AccessRead reads tags or the change feed.
	AccessRead Access = iota
	// AccessWrite writes a tag's value or quality (PUT /tags/{name}).
	AccessWrite
)

// ErrForbidden is returned by a ServerOptions.Authorize function for a caller
// that is authenticated but not allowed the access: the server answers 403.
// Any other error answers 401.
var ErrForbidden = errors.New("honeycomb: forbidden")

// DefaultMaxBodyBytes bounds the body of a write request.
const DefaultMaxBodyBytes = 1 << 20

// Server timeouts. The write timeout leaves room for the longest change-feed
// long poll (MaxChangesWait).
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = MaxChangesWait + 30*time.Second
	DefaultIdleTimeout       = 2 * time.Minute
)

// ServerOptions configures NewServer.
type ServerOptions struct {
	// Addr is the listen address, host:port, e.g. "127.0.0.1:8443". An empty
	// host listens on every interface.
	Addr string
	// Tokens are the valid Bearer tokens, compared in constant time. Ignored
	// when Authorize is set.
	Tokens []string
	// Authorize, when set, decides every request instead of Tokens. It
	// returns the caller's name (passed to OnWrite) or an error: ErrForbidden
	// for 403, anything else for 401.
	Authorize func(r *http.Request, access Access) (subject string, err error)
	// ReadOnly refuses every write with 403.
	ReadOnly bool
	// OnWrite, when set, is called after each successful write, e.g. to
	// journal it.
	OnWrite func(r *http.Request, subject, tag string)
	// MaxBodyBytes bounds a write request's body; 0 means DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// TLSConfig is the server's TLS configuration; nil uses Go's defaults.
	// Certificates may also be given to ListenAndServeTLS.
	TLSConfig *tls.Config
}

// subjectKey carries the authorized caller's name in a request's context.
type subjectKey struct{}

// NewServer returns an http.Server serving db's tag API with the timeouts,
// limits and authentication in opts. Start it with ListenAndServeTLS.
func NewServer(db *TagDatabase, opts ServerOptions) (*http.Server, error) {
	if opts.Addr == "" {
		return nil, errors.New("honeycomb: NewServer needs an address")
	}
	if opts.Authorize == nil && len(opts.Tokens) == 0 {
		return nil, errors.New("honeycomb: NewServer needs tokens or an Authorize function")
	}
	if opts.Authorize == nil {
		for _, t := range opts.Tokens {
			if strings.TrimSpace(t) == "" {
				return nil, errors.New("honeycomb: NewServer was given an empty token (is an environment variable unset?)")
			}
		}
	}
	server := &tagServer{
		db:          db,
		validTokens: opts.Tokens,
		authorize:   opts.Authorize,
		readOnly:    opts.ReadOnly,
		onWrite:     opts.OnWrite,
		maxBody:     opts.MaxBodyBytes,
	}
	mux := http.NewServeMux()
	// Individual tags: GET reads, PUT writes.
	mux.Handle("/tags/", server.authMiddleware(http.HandlerFunc(server.tagHandler)))
	// All tags, or a batch read (batchread.go).
	mux.Handle("/tags", server.authMiddleware(http.HandlerFunc(server.handleGetAllTags)))
	// The change feed (changefeed_network.go): POST /changes, long-polled.
	mux.Handle("/changes", server.authMiddleware(http.HandlerFunc(server.handleChanges)))

	return &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		TLSConfig:         opts.TLSConfig,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		ReadTimeout:       DefaultReadTimeout,
		WriteTimeout:      DefaultWriteTimeout,
		IdleTimeout:       DefaultIdleTimeout,
		MaxHeaderBytes:    32 << 10,
	}, nil
}

// StartServer initializes and runs the HTTPS server in a background goroutine
// on every interface, with Bearer token authentication. For a listen address,
// read-only mode, other authentication or a write hook, use NewServer.
//
// Parameters:
//   - db: The TagDatabase instance that the server will expose.
//   - validTokens: A slice of strings representing the valid Bearer tokens for authentication.
//   - port: The network port on which the server will listen (e.g., "8080").
//   - certFile: The path to the TLS certificate file for enabling HTTPS.
//   - keyFile: The path to the TLS private key file for enabling HTTPS.
//   - serverReady: A *sync.WaitGroup used to signal when the server has completed its
//     initial setup and is about to start listening for connections. The caller can
//     use this to wait until the server is ready before proceeding. It can be nil.
func StartServer(db *TagDatabase, validTokens []string, port, certFile, keyFile string, serverReady *sync.WaitGroup, ctx context.Context) {
	httpServer, err := NewServer(db, ServerOptions{Addr: ":" + port, Tokens: validTokens})
	if err != nil {
		log.Fatalf("[Server] %v", err)
	}
	Serve(ctx, httpServer, certFile, keyFile, serverReady)
}

// Serve runs httpServer with TLS in a background goroutine until ctx is
// done, then shuts it down gracefully. serverReady, if not nil, is marked
// done once the server is listening.
//
// If the address can't be bound or the certificate can't be loaded, Serve
// calls log.Fatalf, which exits the whole process. A program that hosts the
// server beside other work should use ListenAndServeTLS, which returns the
// error instead.
func Serve(ctx context.Context, httpServer *http.Server, certFile, keyFile string, serverReady *sync.WaitGroup) {
	log.Printf("[Server] Starting honeycomb network server on %s...", httpServer.Addr)
	done, err := ListenAndServeTLS(ctx, httpServer, certFile, keyFile)
	if err != nil {
		log.Fatalf("[Server] Could not listen on %s: %v\n", httpServer.Addr, err)
	}
	if serverReady != nil {
		serverReady.Done()
	}
	go func() {
		if err := <-done; err != nil {
			log.Fatalf("[Server] Server failed on %s: %v\n", httpServer.Addr, err)
		}
		log.Println("[Server] Server stopped.")
	}()
}

// ListenAndServeTLS loads the certificate and binds httpServer.Addr before it
// returns, so a busy port or a bad certificate comes back as an error and the
// caller decides what to do. It then serves with TLS in the background until
// ctx is done and shuts down gracefully. The returned channel receives nil
// once the server has stopped after ctx ended, or the error that stopped it.
//
// certFile and keyFile may be empty when httpServer.TLSConfig already holds
// the certificates (Certificates or GetCertificate).
func ListenAndServeTLS(ctx context.Context, httpServer *http.Server, certFile, keyFile string) (<-chan error, error) {
	if certFile != "" || keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("honeycomb: load TLS certificate: %w", err)
		}
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if httpServer.TLSConfig != nil {
			cfg = httpServer.TLSConfig.Clone()
		}
		cfg.Certificates = append([]tls.Certificate{cert}, cfg.Certificates...)
		httpServer.TLSConfig = cfg
	}
	addr := httpServer.Addr
	if addr == "" {
		addr = ":https"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("honeycomb: listen on %s: %w", addr, err)
	}
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		err := httpServer.ServeTLS(ln, "", "")
		close(stopped)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	go func() {
		select {
		case <-ctx.Done():
			log.Println("[Server] Shutdown signal received, shutting down server gracefully...")
			httpServer.Shutdown(context.Background())
		case <-stopped:
		}
	}()
	return done, nil
}
