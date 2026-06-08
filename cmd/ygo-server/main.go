// Command ygo-server is a runnable, production-grade Yjs-compatible WebSocket
// collaboration server. It serves the provider/websocket Server over HTTP,
// optionally persisting every room to a SQLite database (so documents survive
// restarts) and optionally relaying updates through Redis so multiple server
// instances behind a load balancer share one logical document per room.
//
// # Usage
//
//	ygo-server [flags]
//
//	-addr               TCP listen address (default ":1234")
//	-store              SQLite database path; empty = in-memory, no persistence
//	-origins            comma-separated allowed WebSocket origins; empty = same-origin
//	-max-conns          server-wide cap on simultaneous peers (0 = unlimited)
//	-max-peers-per-room per-room peer cap (0 = unlimited)
//	-max-rooms          cap on total resident rooms (0 = unlimited)
//	-max-message-bytes  per-message read cap in bytes (default 64 MiB)
//	-redis              Redis address for cross-process relay; empty = disabled
//	-path               URL path pattern for the WebSocket handler (default "/yjs/{room}")
//	-log                log format: "text" or "json" (default "text")
//
// The process serves until it receives SIGINT or SIGTERM, then shuts down
// gracefully: it stops accepting connections, drains in-flight work, detaches
// the relay (if any) and closes the persistence store.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/reearth/ygo/cluster/redis"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/persistence/sqlite"
	ygows "github.com/reearth/ygo/provider/websocket"
)

// Config is the fully-resolved server configuration. It is produced by
// parseFlags and consumed by run; keeping it a plain struct makes run testable
// without touching os.Args or the process environment.
type Config struct {
	// Addr is the TCP address the HTTP server listens on (e.g. ":1234").
	Addr string
	// Store is the SQLite database path. Empty disables persistence (rooms are
	// in-memory only and lost on restart).
	Store string
	// Origins is the comma-separated list of allowed WebSocket origins. Empty
	// performs a same-origin check; "*" allows any origin.
	Origins string
	// MaxConns is the server-wide cap on simultaneous peers (0 = unlimited).
	MaxConns int
	// MaxPeersPerRoom is the per-room peer cap (0 = unlimited).
	MaxPeersPerRoom int
	// MaxRooms caps the total number of resident rooms (0 = unlimited).
	MaxRooms int
	// MaxMessageBytes is the per-message read cap in bytes.
	MaxMessageBytes int64
	// Redis is the Redis address for the cross-process relay. Empty disables
	// clustering.
	Redis string
	// Path is the URL path pattern the WebSocket handler is mounted at.
	Path string
	// Log is the log format: "text" or "json".
	Log string
}

// parseFlags parses the supplied arguments into a Config. It uses a dedicated
// FlagSet (rather than the global flag.CommandLine) so it is safe to call from
// tests and never calls os.Exit on a parse error — it returns the error so the
// caller decides.
func parseFlags(args []string) (Config, error) {
	fs := flag.NewFlagSet("ygo-server", flag.ContinueOnError)

	var cfg Config
	fs.StringVar(&cfg.Addr, "addr", ":1234", "TCP listen address")
	fs.StringVar(&cfg.Store, "store", "", "SQLite database path (empty = no persistence)")
	fs.StringVar(&cfg.Origins, "origins", "", "comma-separated allowed WebSocket origins (empty = same-origin)")
	fs.IntVar(&cfg.MaxConns, "max-conns", 0, "server-wide cap on simultaneous peers (0 = unlimited)")
	fs.IntVar(&cfg.MaxPeersPerRoom, "max-peers-per-room", 0, "per-room peer cap (0 = unlimited)")
	fs.IntVar(&cfg.MaxRooms, "max-rooms", 0, "cap on total resident rooms (0 = unlimited)")
	fs.Int64Var(&cfg.MaxMessageBytes, "max-message-bytes", 64<<20, "per-message read cap in bytes")
	fs.StringVar(&cfg.Redis, "redis", "", "Redis address for cross-process relay (empty = disabled)")
	fs.StringVar(&cfg.Path, "path", "/yjs/{room}", "URL path pattern for the WebSocket handler")
	fs.StringVar(&cfg.Log, "log", "text", `log format: "text" or "json"`)

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// newLogger builds a slog.Logger writing to stderr in the requested format.
// Any value other than "json" yields a text handler.
func newLogger(format string) *slog.Logger {
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, nil)
	} else {
		h = slog.NewTextHandler(os.Stderr, nil)
	}
	return slog.New(h)
}

// run boots the server and blocks until ctx is cancelled (e.g. by a signal) or
// the HTTP server fails, then shuts everything down gracefully and returns.
//
// ready, when non-nil, is closed once the listener is bound and the server is
// accepting connections — tests use it to learn when it is safe to connect
// without polling. A nil ready (the main() path) skips the signal.
func run(ctx context.Context, cfg Config, ready chan<- struct{}) error {
	log := newLogger(cfg.Log)

	// Default the handler path here (not only in parseFlags): run is a public
	// seam called directly by tests with a bare Config, and an empty pattern
	// makes http.ServeMux.Handle panic.
	if cfg.Path == "" {
		cfg.Path = "/yjs/{room}"
	}

	// Persistence: a non-empty Store wires SQLite via the LegacyAdapter so every
	// committed transaction becomes a persisted version and rooms reload on
	// restart. store is owned here and closed on shutdown.
	var (
		srv   *ygows.Server
		store *sqlite.Store
	)
	if cfg.Store != "" {
		s, err := sqlite.Open(cfg.Store)
		if err != nil {
			return err
		}
		store = s
		srv = ygows.NewServerWithPersistence(persistence.NewLegacyAdapter(store))
	} else {
		srv = ygows.NewServer()
	}

	if cfg.Origins != "" {
		srv.AllowedOrigins = strings.Split(cfg.Origins, ",")
	}
	srv.MaxConnections = cfg.MaxConns
	srv.MaxPeersPerRoom = cfg.MaxPeersPerRoom
	srv.MaxRooms = cfg.MaxRooms
	srv.MaxMessageBytes = cfg.MaxMessageBytes
	srv.Logger = log

	// Clustering: a non-empty Redis address attaches a Redis-backed relay so
	// multiple server instances share one logical document per room. AttachRelay
	// Starts the relay internally; the caller owns relay.Close (Server.Shutdown
	// only cancels this server's delivery), so we close it ourselves on shutdown.
	var relay *redis.Relay
	if cfg.Redis != "" {
		client := goredis.NewClient(&goredis.Options{Addr: cfg.Redis})
		r, err := redis.New(client, redis.Config{})
		if err != nil {
			_ = client.Close()
			if store != nil {
				_ = store.Close()
			}
			return err
		}
		if err := srv.AttachRelay(r); err != nil {
			_ = r.Close()
			_ = client.Close()
			if store != nil {
				_ = store.Close()
			}
			return err
		}
		relay = r
	}

	mux := http.NewServeMux()
	mux.Handle(cfg.Path, srv)
	hsrv := &http.Server{Addr: cfg.Addr, Handler: mux}

	// Bind the listener explicitly (rather than relying on hsrv.ListenAndServe)
	// so the server is provably accepting connections before we signal ready —
	// tests connect on ":0"-assigned ports and must not race the bind.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		if relay != nil {
			_ = relay.Close()
		}
		if store != nil {
			_ = store.Close()
		}
		return err
	}

	log.Info("listening", "addr", ln.Addr().String(), "path", cfg.Path,
		"store", cfg.Store, "redis", cfg.Redis)
	if ready != nil {
		close(ready)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- hsrv.Serve(ln) }()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Best-effort cleanup before surfacing the serve failure.
			if relay != nil {
				_ = relay.Close()
			}
			if store != nil {
				_ = store.Close()
			}
			return err
		}
	}

	// Graceful shutdown: stop accepting and drain in-flight HTTP, then stop the
	// server's delivery, then close the relay (we own it) and the store.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hsrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("server shutdown", "err", err)
	}
	if relay != nil {
		if err := relay.Close(); err != nil {
			log.Warn("relay close", "err", err)
		}
	}
	if store != nil {
		if err := store.Close(); err != nil {
			log.Warn("store close", "err", err)
		}
	}
	return nil
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		// flag already printed the usage/error to stderr; exit code 2 matches
		// the conventional "bad command-line usage" status.
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, nil); err != nil {
		slog.Error("ygo-server exited with error", "err", err)
		os.Exit(1)
	}
}
