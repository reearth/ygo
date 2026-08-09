// Command ygo-server is a runnable, production-grade Yjs-compatible WebSocket
// collaboration server. It serves the provider/websocket Server over HTTP,
// persisting every room to a SQLite database (a durable file so documents
// survive restarts, or an ephemeral in-memory store) and optionally relaying
// updates through Redis so multiple server instances behind a load balancer
// share one logical document per room.
//
// # Usage
//
//	ygo-server [flags]
//
//	-addr               TCP listen address (default "127.0.0.1:1234", loopback-only;
//	                    a non-loopback bind exposes the server, which has no built-in auth)
//	-store              SQLite DB path; empty = ephemeral in-memory (lost on restart)
//	-origins            comma-separated allowed WebSocket origins; empty = same-origin
//	-max-conns          server-wide cap on simultaneous peers (0 = unlimited)
//	-max-peers-per-room per-room peer cap (0 = unlimited)
//	-max-rooms          cap on total resident rooms (0 = unlimited)
//	-max-message-bytes  per-message read cap in bytes (default 64 MiB)
//	-max-awareness-clients
//	                    per-room cap on distinct awareness client entries
//	                    (default 10000; 0 = unlimited)
//	-awareness-expiry   reclaim a remote client's presence after this idle
//	                    duration (default 30s; 0 = disabled)
//	-redis              Redis address for cross-process relay; empty = disabled
//	-path               URL path pattern for the WebSocket handler (default "/yjs/{room}")
//	-log                log format: "text" or "json" (default "text")
//	-version-interval   capture a version of each CHANGED room at most this
//	                    often; a quiet room is never re-versioned (0 = disabled)
//	-keep-snapshots     retain this many auto-captured versions per room;
//	                    applied on capture, so it needs -version-interval
//	                    (0 = keep all)
//
// The process serves until it receives SIGINT or SIGTERM, then shuts down
// gracefully: it stops accepting connections, drains in-flight work, detaches
// the relay (if any) and closes the persistence store.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
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
	// Store is the SQLite database path. Empty opens an ephemeral in-memory
	// database: rooms persist across room eviction within the process but are
	// lost on restart.
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
	// MaxAwarenessClients caps the distinct awareness client entries per room
	// (live presence plus removal tombstones). Bounds memory against a peer that
	// invents unbounded client IDs. 0 = unlimited.
	MaxAwarenessClients int
	// AwarenessExpiry reclaims a remote client's presence if no update arrives
	// within this duration (ghost-presence cleanup). 0 = disabled.
	AwarenessExpiry time.Duration
	// Redis is the Redis address for the cross-process relay. Empty disables
	// clustering.
	Redis string
	// Path is the URL path pattern the WebSocket handler is mounted at.
	Path string
	// Log is the log format: "text" or "json".
	Log string
	// VersionInterval enables periodic version capture: each room that CHANGED
	// since its last version is captured at most this often, giving a
	// user-facing history without the application driving one. A quiet room is
	// never re-versioned. 0 = disabled.
	VersionInterval time.Duration
	// KeepSnapshots bounds retained auto-captured versions per room. It is
	// applied when a version is captured, so it does nothing without
	// VersionInterval. Retention is per LABEL, so it only ever trims the
	// server's own auto versions and cannot evict a snapshot an application
	// named deliberately. 0 = keep all.
	KeepSnapshots int
}

// parseFlags parses the supplied arguments into a Config. It uses a dedicated
// FlagSet (rather than the global flag.CommandLine) so it is safe to call from
// tests and never calls os.Exit on a parse error — it returns the error so the
// caller decides.
func parseFlags(args []string) (Config, error) {
	var cfg Config
	fs := newFlagSet(&cfg)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// newFlagSet declares every flag against cfg and returns the FlagSet without
// parsing. Split out from parseFlags so a test can enumerate the flags via
// VisitAll and assert the package doc's Usage block lists all of them — two
// flags had already drifted out of that block before this seam existed.
func newFlagSet(cfg *Config) *flag.FlagSet {
	fs := flag.NewFlagSet("ygo-server", flag.ContinueOnError)

	fs.StringVar(&cfg.Addr, "addr", "127.0.0.1:1234",
		"TCP listen address (default loopback-only; a non-loopback bind exposes the server, which has no built-in auth)")
	fs.StringVar(&cfg.Store, "store", "", "SQLite DB path (empty = ephemeral in-memory; lost on restart)")
	fs.StringVar(&cfg.Origins, "origins", "", "comma-separated allowed WebSocket origins (empty = same-origin)")
	fs.IntVar(&cfg.MaxConns, "max-conns", 0, "server-wide cap on simultaneous peers (0 = unlimited)")
	fs.IntVar(&cfg.MaxPeersPerRoom, "max-peers-per-room", 0, "per-room peer cap (0 = unlimited)")
	fs.IntVar(&cfg.MaxRooms, "max-rooms", 0, "cap on total resident rooms (0 = unlimited)")
	fs.Int64Var(&cfg.MaxMessageBytes, "max-message-bytes", 64<<20, "per-message read cap in bytes")
	fs.IntVar(&cfg.MaxAwarenessClients, "max-awareness-clients", 10_000, "per-room cap on distinct awareness client entries (0 = unlimited)")
	fs.DurationVar(&cfg.AwarenessExpiry, "awareness-expiry", 30*time.Second, "reclaim a remote client's presence after this idle duration (0 = disabled)")
	fs.StringVar(&cfg.Redis, "redis", "", "Redis address for cross-process relay (empty = disabled)")
	fs.StringVar(&cfg.Path, "path", "/yjs/{room}", "URL path pattern for the WebSocket handler")
	fs.StringVar(&cfg.Log, "log", "text", `log format: "text" or "json"`)
	fs.DurationVar(&cfg.VersionInterval, "version-interval", 0,
		"capture a version of each CHANGED room at most this often; a quiet room is never re-versioned (0 = disabled)")
	fs.IntVar(&cfg.KeepSnapshots, "keep-snapshots", 0,
		"retain this many auto-captured versions per room; applied on capture, so it needs -version-interval (0 = keep all)")

	return fs
}

// newLogger builds a slog.Logger writing to stderr in the requested format.
// Any value other than "json" yields a text handler.
func newLogger(format string) *slog.Logger { return newLoggerTo(os.Stderr, format) }

// newLoggerTo builds the logger writing to w. "json" selects a JSON handler;
// anything else (incl. "text" and unknown values) selects a text handler. The
// writer seam keeps the format selection unit-testable.
func newLoggerTo(w io.Writer, format string) *slog.Logger {
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(w, nil)
	} else {
		h = slog.NewTextHandler(w, nil)
	}
	return slog.New(h)
}

// parseOrigins splits a comma-separated origins flag into a slice, trimming
// surrounding whitespace from each entry and dropping empties. This ensures a
// human-friendly value like "https://a.com, https://b.com" does not yield a
// " https://b.com" entry that never matches an incoming Origin header. Returns
// nil for an empty/whitespace-only string (which leaves the same-origin default).
func parseOrigins(s string) []string {
	var origins []string
	for _, o := range strings.Split(s, ",") {
		if t := strings.TrimSpace(o); t != "" {
			origins = append(origins, t)
		}
	}
	return origins
}

// isPublicBindAddr reports whether addr — a TCP listen address such as
// ":1234", "0.0.0.0:1234", or "127.0.0.1:1234" — binds a non-loopback
// interface, making the server reachable from other hosts. An empty host
// (":1234") or a wildcard address (0.0.0.0 / ::) binds every interface and is
// therefore public; a loopback host (127.0.0.1, ::1, localhost) is private.
// Any other host (a LAN IP, a public IP, or a DNS name we cannot prove is
// loopback) is treated as public so the security warning errs toward caution.
func isPublicBindAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port (or otherwise unparseable): treat the whole value as the host.
		host = addr
	}
	switch host {
	case "":
		return true // ":1234" binds all interfaces
	case "localhost":
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return true
}

// warnIfInertRetention logs when -keep-snapshots is set without
// -version-interval. Retention is applied at the moment a version is captured,
// so with nothing capturing versions the bound is never enforced — an operator
// who asked for one would otherwise believe it was in force. Same reasoning as
// warnIfInsecureBind: a configuration that silently does nothing deserves to
// say so at startup, where it will actually be read.
func warnIfInertRetention(log *slog.Logger, cfg Config) {
	if cfg.KeepSnapshots <= 0 || cfg.VersionInterval > 0 {
		return
	}
	log.Warn("-keep-snapshots has no effect without -version-interval: " +
		"retention is applied when a version is captured, and nothing captures versions")
}

// warnIfInsecureBind logs a prominent warning when addr exposes the server to
// the network. ygo-server wires no authentication of its own, so a non-loopback
// bind means any host that can reach the port can read and modify every
// document. The default bind is loopback, so this fires only when an operator
// overrides -addr — at which point the deployment is expected to provide auth
// at a layer in front (e.g. an authenticating reverse proxy).
func warnIfInsecureBind(log *slog.Logger, addr string) {
	if !isPublicBindAddr(addr) {
		return
	}
	log.Warn("SECURITY: ygo-server is bound to a non-loopback address with NO authentication — "+
		"any host that can reach this port can read and modify every document. Bind to 127.0.0.1 "+
		"(the default), front it with an authenticating reverse proxy, or restrict access at the "+
		"network layer.", "addr", addr)
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

	// S-2: the server has no built-in authentication, so a non-loopback bind is
	// only safe behind a separate auth layer. Warn loudly when that is the case
	// (the default bind is loopback, so this stays silent for a default run).
	warnIfInsecureBind(log, cfg.Addr)

	// Persistence: always wire SQLite via the LegacyAdapter so every committed
	// transaction becomes a persisted version. An empty Store opens an ephemeral
	// in-memory database (room state survives room eviction within the process but
	// is lost on restart); a non-empty Store reloads rooms across restarts. store
	// is owned here and closed on shutdown.
	store, err := sqlite.Open(cfg.Store) // "" => ephemeral in-memory
	if err != nil {
		return err
	}
	// The adapter is named rather than inlined because auto-versioning splits
	// across both objects: the capture cadence is the server's concern
	// (AutoVersionEvery) while retention is the adapter's (KeepSnapshots). Both
	// are set before serving, as KeepSnapshots' contract requires.
	adapter := persistence.NewLegacyAdapter(store)
	adapter.KeepSnapshots = cfg.KeepSnapshots
	srv := ygows.NewServerWithPersistence(adapter)
	srv.AutoVersionEvery = cfg.VersionInterval

	warnIfInertRetention(log, cfg)

	srv.AllowedOrigins = parseOrigins(cfg.Origins)
	srv.MaxConnections = cfg.MaxConns
	srv.MaxPeersPerRoom = cfg.MaxPeersPerRoom
	srv.MaxRooms = cfg.MaxRooms
	srv.MaxMessageBytes = cfg.MaxMessageBytes
	srv.MaxAwarenessClientsPerRoom = cfg.MaxAwarenessClients
	srv.AwarenessExpiry = cfg.AwarenessExpiry
	srv.Logger = log

	// Clustering: a non-empty Redis address attaches a Redis-backed relay so
	// multiple server instances share one logical document per room. AttachRelay
	// Starts the relay internally; the caller owns relay.Close (Server.Shutdown
	// only cancels this server's delivery), so we close it ourselves on shutdown.
	var relay *redis.Relay
	if cfg.Redis != "" {
		client := goredis.NewClient(&goredis.Options{Addr: cfg.Redis})
		// client is owned here for the lifetime of run; close it on every exit
		// path (happy shutdown and the error paths below) so its connection pool
		// is released even when run is invoked repeatedly in-process (tests).
		defer func() { _ = client.Close() }()
		r, err := redis.New(client, redis.Config{})
		if err != nil {
			_ = store.Close()
			return err
		}
		if err := srv.AttachRelay(r); err != nil {
			_ = r.Close()
			_ = store.Close()
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
		_ = store.Close()
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
			// Best-effort cleanup before surfacing the serve failure. srv.Shutdown
			// is skipped here because returning this error makes main() exit
			// fatally (os.Exit(1)); the deferred client.Close() still runs.
			if relay != nil {
				_ = relay.Close()
			}
			_ = store.Close()
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
	if err := store.Close(); err != nil {
		log.Warn("store close", "err", err)
	}
	return nil
}

func main() { os.Exit(realMain(os.Args[1:])) }

// realMain runs the CLI and returns the process exit code. Split out from main
// so the argument-parsing exit codes are unit-testable without spawning a
// process. Exit codes: 0 = clean (incl. -h/--help), 1 = runtime error,
// 2 = bad command-line usage.
func realMain(args []string) int {
	cfg, err := parseFlags(args)
	if err != nil {
		// -h/--help is an explicit request, not an error: flag already printed
		// usage to stderr; exit 0 per CLI convention.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		// flag already printed the usage/error to stderr; 2 = bad usage.
		return 2
	}

	// Align the package default logger with the chosen format so the fatal-exit
	// log below (and any other default-logger use) honors -log.
	slog.SetDefault(newLogger(cfg.Log))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, nil); err != nil {
		slog.Error("ygo-server exited with error", "err", err)
		return 1
	}
	return 0
}
