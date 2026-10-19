package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/gravity-zero/mkvgo/mkvhttp"
	"github.com/gravity-zero/mkvgo/mp4"
)

// refreshInterval is how often CmdServeGrowing re-stats the source and scans
// any new whole clusters. A still-downloading file grows in bursts (whatever
// the network delivers); polling every second is cheap (a stat plus, at most,
// a bounded read of the new clusters) and keeps the playlist close to
// realtime without a filesystem watch dependency (zero external deps).
const refreshInterval = 1 * time.Second

// CmdServeGrowing serves one file that may still be growing (a download in
// progress) as a play-while-downloading HLS presentation over HTTP
// (mp4.PlanGrowingHLS + mkvhttp.Handler): the media playlist lengthens as new
// whole clusters land, then switches to VOD+ENDLIST once the source finishes
// (detected automatically, e.g. a Cues index appearing at the end). Ctrl-C
// shuts the server down gracefully.
func CmdServeGrowing(args []string) {
	cfg := parseServeGrowingFlags(args)
	addr, segMs, rest := cfg.addr, cfg.segMs, cfg.rest
	if len(rest) < 1 {
		Fatal("usage: " + CmdUsage["serve-growing"])
	}
	src := rest[0]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	plan, err := mp4.PlanGrowingHLS(ctx, src, mp4.Options{SegmentMs: segMs})
	if err != nil {
		FatalErr(err)
	}

	srv := newServer(addr, mkvhttp.Handler(plan, mkvhttp.Options{AllowCORS: cfg.cors}), segmentWriteTimeout)

	fmt.Printf("serving %s (play while downloading)\n  %s\n", src, playableURL(addr))
	fmt.Println("press Ctrl-C to stop")

	go refreshLoop(ctx, plan)

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
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			FatalErr(err)
		}
	}
}

// refreshLoop polls the growing plan for new whole clusters until ctx is
// cancelled (server shutdown). A Refresh error is logged, not fatal: a
// transient read hiccup on a still-downloading file should not bring the
// server down mid-stream.
func refreshLoop(ctx context.Context, plan *mp4.GrowingHLSPlan) {
	t := time.NewTicker(refreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := plan.Refresh(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "refresh: %v\n", err)
			}
		}
	}
}

// serveGrowingFlags are serve-growing's flags.
type serveGrowingFlags struct {
	addr  string
	segMs int64
	cors  bool
	rest  []string
}

// parseServeGrowingFlags reads serve-growing's flags.
func parseServeGrowingFlags(args []string) serveGrowingFlags {
	cfg := serveGrowingFlags{addr: defaultServeAddr}
	fl := newFlags("serve-growing")
	fl.String(&cfg.addr, "-addr", "--addr")
	fl.Bool(&cfg.cors, "-cors", "--cors")
	fl.Func(func(v string) {
		cfg.segMs = parseSegmentSeconds(v)
	}, "-segment", "--segment")
	cfg.rest = fl.Parse(args)
	return cfg
}
