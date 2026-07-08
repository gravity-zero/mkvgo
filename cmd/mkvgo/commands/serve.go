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

// CmdServe serves one file's on-demand HLS plan over HTTP: nothing is
// pre-generated on disk - master/media playlists, init segments, media
// segments and subtitle renditions are each built the first time a player
// asks for them, straight from the source (mp4.PlanHLS + mkvhttp.Handler).
// Accepts the same shared HLS flags as to-hls/hls-segment (--segment,
// --keep-tracks, --keep-lang, ...). Ctrl-C shuts the server down gracefully.
func CmdServe(args []string) {
	addr := ":8478"
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-addr", "--addr":
			i++
			if i < len(args) {
				addr = args[i]
			}
		default:
			rest = append(rest, args[i])
		}
	}

	f := parseHLSFlags(rest)
	if len(f.rest) < 1 {
		Fatal("usage: " + CmdUsage["serve"])
	}
	src := f.rest[0]

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
	host := addr
	if len(host) > 0 && host[0] == ':' {
		host = "localhost" + host
	}
	return "http://" + host + "/master.m3u8"
}
