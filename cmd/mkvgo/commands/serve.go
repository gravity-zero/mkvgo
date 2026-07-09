package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	addr := ":8478"
	direct := false
	auto := false
	targetName := "mse-generic"
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-addr", "--addr":
			i++
			if i < len(args) {
				addr = args[i]
			}
		case "-direct", "--direct":
			direct = true
		case "-auto", "--auto":
			auto = true
		case "-target", "--target":
			i++
			if i < len(args) {
				targetName = args[i]
			}
		default:
			rest = append(rest, args[i])
		}
	}
	if direct && auto {
		Fatal("serve: --direct and --auto are mutually exclusive")
	}

	f := parseHLSFlags(rest)
	if len(f.rest) < 1 {
		Fatal("usage: " + CmdUsage["serve"])
	}
	src := f.rest[0]

	if auto {
		target, ok := matroska.TargetByName(targetName)
		if !ok {
			Fatal(fmt.Sprintf("unknown target %q", targetName))
		}
		report, err := matroska.Playability(context.Background(), src, target)
		if err != nil {
			Fatal(err.Error())
		}
		direct = report.OverallVerdict == directPlayVerdict
		fmt.Printf("auto: %s verdict for target %q -> %s\n", report.OverallVerdict, targetName, serveModeLabel(direct))
	}

	if direct {
		serveDirect(addr, src)
		return
	}

	opts := f.options(src)
	opts.OnDrop = func(d mp4.DroppedTrack) {
		fmt.Fprintf(os.Stderr, "dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
	}
	plan, err := mp4.PlanHLS(context.Background(), src, opts)
	if err != nil {
		Fatal(err.Error())
	}

	srv := &http.Server{Addr: addr, Handler: mkvhttp.Handler(plan, mkvhttp.Options{AllowCORS: true})}

	fmt.Printf("serving %s\n  %s\n", src, playableURL(addr))
	runServer(srv)
}

// serveDirect serves src as-is over HTTP for a direct-play client: no
// packaging, just byte-range access to the raw file (mkvhttp.FileHandler).
func serveDirect(addr, src string) {
	name := filepath.Base(src)
	mux := http.NewServeMux()
	mux.Handle("/"+name, mkvhttp.FileHandler(src, mkvhttp.Options{AllowCORS: true}))
	srv := &http.Server{Addr: addr, Handler: mux}

	fmt.Printf("serving %s (direct-play, no packaging)\n  %s\n", src, directPlayableURL(addr, name))
	runServer(srv)
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			Fatal(err.Error())
		}
	}
}

// playableURL turns a listen address (":8478", "0.0.0.0:8478", "127.0.0.1:8478")
// into a URL a player can actually connect to - a bare port needs a host.
func playableURL(addr string) string {
	return "http://" + hostOf(addr) + "/master.m3u8"
}

// directPlayableURL is playableURL's counterpart for --direct/--auto: the
// file is mounted at its own base name instead of a fixed playlist name.
func directPlayableURL(addr, name string) string {
	return "http://" + hostOf(addr) + "/" + name
}

// hostOf turns a bare-port listen address into one a player can connect to.
func hostOf(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "localhost" + addr
	}
	return addr
}
