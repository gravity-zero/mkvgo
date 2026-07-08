package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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
	addr := ":8478"
	segMs := int64(0)
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-addr", "--addr":
			i++
			if i < len(args) {
				addr = args[i]
			}
		case "-segment", "--segment":
			i++
			if i < len(args) {
				secs, err := strconv.ParseFloat(args[i], 64)
				if err != nil || secs <= 0 {
					Fatal(fmt.Sprintf("invalid -segment duration %q (seconds)", args[i]))
				}
				segMs = int64(secs * 1000)
			}
		default:
			rejectFlagArg(args[i])
			rest = append(rest, args[i])
		}
	}
	if len(rest) < 1 {
		Fatal("usage: " + CmdUsage["serve-growing"])
	}
	src := rest[0]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	plan, err := mp4.PlanGrowingHLS(ctx, src, mp4.Options{SegmentMs: segMs})
	if err != nil {
		Fatal(err.Error())
	}

	srv := &http.Server{Addr: addr, Handler: mkvhttp.Handler(plan, mkvhttp.Options{AllowCORS: true})}

	fmt.Printf("serving %s (play while downloading)\n  %s\n", src, playableURL(addr))
	fmt.Println("press Ctrl-C to stop")

	go refreshLoop(ctx, plan)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			Fatal(err.Error())
		}
	case <-sigCh:
		fmt.Println("\nshutting down...")
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			Fatal(err.Error())
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
