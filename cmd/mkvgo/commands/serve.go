package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkvhttp"
	"github.com/gravity-zero/mkvgo/mp4"
)

// directPlayVerdict is the matroska.PlayabilityReport.OverallVerdict string
// that means "serve the file as-is, no packaging needed".
const directPlayVerdict = "direct-play"

// CmdServe serves one file over HTTP, either its on-demand HLS plan (the
// default) or the raw file itself for a client that can direct-play it
// (--direct), or lets a Playability verdict pick between the two (--auto,
// against --target, default mse-generic). Nothing is pre-generated on disk
// in the HLS case - master/media playlists, init segments, media segments
// and subtitle renditions are each built the first time a player asks for
// them, straight from the source (mp4.PlanHLS + mkvhttp.Handler). Accepts
// the same shared HLS flags as to-hls/hls-segment (--segment, --keep-tracks,
// --keep-lang, ...) when packaging. Ctrl-C shuts the server down gracefully.
func CmdServe(args []string) {
	cfg := parseServeFlags(args)
	cfg.applyMemoryLimit()
	addr, direct, auto, targetName := cfg.addr, cfg.direct, cfg.auto, cfg.target
	rest := cfg.rest
	if direct && auto {
		Fatal("serve: --direct and --auto are mutually exclusive")
	}

	f := parseHLSFlags(rest)
	if len(f.rest) < 1 {
		Fatal("usage: " + CmdUsage["serve"])
	}
	f.randomCENCIV()
	src := f.rest[0]

	if auto {
		target, ok := matroska.TargetByName(targetName)
		if !ok {
			Fatal(fmt.Sprintf("unknown target %q", targetName))
		}
		report, err := matroska.Playability(context.Background(), src, target)
		if err != nil {
			FatalErr(err)
		}
		direct = report.OverallVerdict == directPlayVerdict
		fmt.Printf("auto: %s verdict for target %q -> %s\n", report.OverallVerdict, targetName, serveModeLabel(direct))
	}

	if direct {
		serveDirect(addr, src, cfg.httpOptions(cfg.cors))
		return
	}

	opts := f.options(src)
	opts.WindowCacheBytes = cfg.windowCache
	opts.StreamFromFS = true // a remote source streams too: a segment held whole per request grew without bound past capacity
	mp4.SetMaxConcurrentWalks(cfg.maxWalks)
	mp4.SetWindowBytes(cfg.windowBytes)
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Fprintf(os.Stderr, "dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	plan, err := mp4.PlanHLS(context.Background(), src, opts)
	if err != nil {
		FatalErr(err)
	}

	srv := newServer(addr, mkvhttp.Handler(plan, cfg.httpOptions(cfg.cors)), segmentWriteTimeout)

	fmt.Printf("serving %s\n  %s\n", src, playableURL(addr))
	runServer(srv)
}

// serveDirect serves src as-is over HTTP for a direct-play client: no
// packaging, just byte-range access to the raw file (mkvhttp.FileHandler).
func serveDirect(addr, src string, opts mkvhttp.Options) {
	name := filepath.Base(src)
	srv := newServer(addr, directMux(name, mkvhttp.FileHandler(src, mkvhttp.Options{AllowCORS: opts.AllowCORS})), 0) // a direct-play response is the whole film: no deadline on writing it

	fmt.Printf("serving %s (direct-play, no packaging)\n  %s\n", src, directPlayableURL(addr, name))
	runServer(srv)
}

// segmentWriteTimeout bounds writing one HLS response: a segment is at most 256 MiB, so a client stalled past this
// releases the source handle and the window budget its segment holds.
const segmentWriteTimeout = 5 * time.Minute

// newServer is an http.Server whose clients cannot hold a connection open without sending or reading: a header must
// arrive within 10 s and an idle connection closes after 2 min; writeTimeout > 0 bounds writing a response.
func newServer(addr string, h http.Handler, writeTimeout time.Duration) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, WriteTimeout: writeTimeout}
}

// defaultServeAddr keeps serve and serve-growing on this machine; -addr :8478 opens them to the network.
const defaultServeAddr = "127.0.0.1:8478"

// serveFlags are the serve command's own flags; rest keeps the shared HLS flags and the source.
type serveFlags struct {
	addr, target   string
	direct, auto   bool
	windowCache    int64 // what the plan may hold for a window's un-collected renditions; 0 sizes it from the source
	windowBytes    int64 // the kept video the process holds for Open (0 = default, -1 = off)
	maxWalks       int   // windows built at once, GOMAXPROCS unless --max-walks says otherwise (0 = no cap)
	maxInFlight    int   // segments served at once (0 = no cap)
	maxWaiting     int   // requests queued behind them before a 503
	memoryLimit    int64 // the Go runtime's soft memory limit in bytes (0 = left as GOMEMLIMIT sets it)
	buffered, cors bool
	rest           []string
}

// parseServeFlags reads serve's flags; the shared HLS flags are left in rest.
func parseServeFlags(args []string) serveFlags {
	cfg := serveFlags{addr: defaultServeAddr, target: "mse-generic", maxWalks: runtime.GOMAXPROCS(0), maxInFlight: defaultMaxInFlight, maxWaiting: defaultMaxWaiting}
	fl := newFlags("serve")
	fl.String(&cfg.addr, "-addr", "--addr")
	fl.Bool(&cfg.direct, "-direct", "--direct")
	fl.Bool(&cfg.auto, "-auto", "--auto")
	fl.String(&cfg.target, "-target", "--target")
	fl.Bool(&cfg.buffered, "-buffered", "--buffered")
	fl.Bool(&cfg.cors, "-cors", "--cors")
	fl.Func(func(v string) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			Fatal("-max-walks: expected a non-negative count, got " + v)
		}
		cfg.maxWalks = n
	}, "-max-walks", "--max-walks")
	fl.Func(func(v string) { cfg.maxInFlight = countFlag(v, "--max-in-flight") }, "-max-in-flight", "--max-in-flight")
	fl.Func(func(v string) { cfg.maxWaiting = countFlag(v, "--max-waiting") }, "-max-waiting", "--max-waiting")
	fl.Func(func(v string) { cfg.windowCache = mibOrOff(v, "--window-cache") }, "-window-cache", "--window-cache")
	fl.Func(func(v string) { cfg.windowBytes = mibOrOff(v, "--window-bytes") }, "-window-bytes", "--window-bytes")
	fl.Func(func(v string) {
		mib, err := strconv.ParseInt(v, 10, 64)
		if err != nil || mib <= 0 {
			Fatal("serve: --memory-limit takes a size in MiB, more than 0 (the budget of --window-bytes plus 64 bounds the peak)")
		}
		cfg.memoryLimit = mib << 20
	}, "-memory-limit", "--memory-limit")
	cfg.rest = fl.ParseKnown(args) // the shared HLS flags and the source, for parseHLSFlags
	return cfg
}

// Default admission: segments served at once, and requests queued behind them before a 503.
const (
	defaultMaxInFlight = 64
	defaultMaxWaiting  = 1024
)

// applyMemoryLimit sets the runtime's soft memory limit on --memory-limit: the collector then runs as the heap nears
// it instead of at twice what is live, kept windows included.
func (cfg serveFlags) applyMemoryLimit() {
	if cfg.memoryLimit > 0 {
		debug.SetMemoryLimit(cfg.memoryLimit)
	}
}

// countFlag reads a non-negative count.
func countFlag(v, flag string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		Fatal("serve: " + flag + ": expected a non-negative count, got " + v)
	}
	return n
}

// mibOrOff reads a size in MiB as bytes, or "off" as -1.
func mibOrOff(v, flag string) int64 {
	if v == "off" {
		return -1
	}
	mib, err := strconv.ParseInt(v, 10, 64)
	if err != nil || mib < 0 {
		Fatal("serve: " + flag + " takes a size in MiB, or \"off\"")
	}
	return mib << 20
}

// httpOptions are the handler options serve runs with: CORS only on --cors.
func (cfg serveFlags) httpOptions(cors bool) mkvhttp.Options {
	return mkvhttp.Options{AllowCORS: cors, Buffered: cfg.buffered, MaxInFlight: cfg.maxInFlight, MaxWaiting: cfg.maxWaiting}
}

// serveModeLabel names the server mode --auto picked, for the log line.
func serveModeLabel(direct bool) string {
	if direct {
		return "direct file (no packaging)"
	}
	return "on-demand HLS"
}

// runServer starts srv and blocks until it exits or Ctrl-C requests a
// graceful shutdown - the part CmdServe and serveDirect share.
func runServer(srv *http.Server) {
	fmt.Println("press Ctrl-C to stop")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt) // before serving, so a Ctrl-C sent once a request is answered reaches this server
	defer signal.Stop(sigCh)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			FatalErr(err)
		}
	case <-sigCh:
		fmt.Println("\nshutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			FatalErr(err)
		}
	}
}

// playableURL turns a listen address (":8478", "0.0.0.0:8478", "127.0.0.1:8478")
// into a URL a player can actually connect to - a bare port needs a host.
func playableURL(addr string) string {
	return "http://" + hostOf(addr) + "/master.m3u8"
}

// directMux serves h at /name only, matched on the decoded path: a ServeMux pattern would read a space or a brace in
// a file's name as pattern syntax and panic.
func directMux(name string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+name {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// directPlayableURL is playableURL's counterpart for --direct/--auto: the
// file is mounted at its own base name instead of a fixed playlist name.
func directPlayableURL(addr, name string) string {
	return "http://" + hostOf(addr) + "/" + url.PathEscape(name)
}

// hostOf turns a bare-port listen address into one a player can connect to.
func hostOf(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "localhost" + addr
	}
	return addr
}
