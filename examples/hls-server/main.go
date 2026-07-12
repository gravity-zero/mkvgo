// Command hls-server is a complete, runnable example: an on-demand HLS + DASH
// streaming server built on mp4.PlanHLS. It packages a Matroska/WebM or MP4/MOV
// file - local path or http(s):// URL (S3) - into CMAF on the fly. Nothing is
// pre-generated: every playlist, manifest and segment is built when a player
// requests it, so first-play latency is milliseconds and storage cost is zero.
//
//	go run ./examples/hls-server -src movie.mkv
//	go run ./examples/hls-server -src https://nas.local/movie.mp4 -addr :9000
//
// Then open http://localhost:8080/ and pick a player, or point any HLS/DASH
// client at:
//
//	http://localhost:8080/hls/master.m3u8    (HLS  - hls.js, Safari)
//	http://localhost:8080/hls/manifest.mpd   (DASH - dash.js)
//
// The whole server is the plan() helper and one handler around
// plan.Resource(name). That is the entire integration surface.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"path"
	"time"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/mp4"
)

func main() {
	src := flag.String("src", "", "source file (path or http(s):// URL) - MKV/WebM or MP4/MOV")
	addr := flag.String("addr", ":8080", "listen address")
	segment := flag.Float64("segment", 6, "target segment duration, seconds")
	flag.Parse()
	if *src == "" {
		log.Fatal("usage: hls-server -src <file|url> [-addr :8080] [-segment 6]")
	}

	// One plan per source. It is immutable and safe for concurrent Resource
	// calls, so a real server would cache one plan per title (keyed by URL);
	// here we build a single plan at startup.
	opts := mp4.Options{SegmentMs: int64(*segment * 1000)}
	if httpfs.IsURL(*src) {
		opts.FS = httpfs.New().Port() // read the remote source over HTTP Range
	}
	plan, err := mp4.PlanHLS(context.Background(), *src, opts)
	if err != nil {
		log.Fatalf("plan %q: %v", *src, err)
	}
	log.Printf("packaging %q - %d segments, %d resources", *src, plan.NumSegments(), len(plan.Resources()))

	mux := http.NewServeMux()

	// The whole streaming endpoint: name → bytes + Content-Type. The name is
	// exactly the URI a player requests (master.m3u8, manifest.mpd,
	// seg00042.m4s, audio1.m3u8, sub1.vtt, iframe.m3u8, …).
	mux.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.URL.Path)
		data, contentType, err := plan.Resource(r.Context(), name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Access-Control-Allow-Origin", "*") // let a browser player fetch cross-origin
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		_, _ = w.Write(data)
	})

	// A tiny landing page that plays the output in real players (hls.js and
	// dash.js from a CDN) - proof both manifests work end to end.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, landingPage)
	})

	log.Printf("serving on http://localhost%s  (HLS: /hls/master.m3u8 · DASH: /hls/manifest.mpd)", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

const landingPage = `<!doctype html>
<meta charset="utf-8">
<title>mkvgo · on-demand HLS + DASH</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 60rem; margin: 2rem auto; padding: 0 1rem; }
  video { width: 100%; background: #000; margin-top: 1rem; }
  button { padding: .5rem 1rem; margin-right: .5rem; font-size: 1rem; }
  code { background: #f3f3f3; padding: .1rem .3rem; border-radius: 3px; }
</style>
<h1>mkvgo · on-demand HLS + DASH</h1>
<p>Segments are built on demand by <code>mp4.PlanHLS</code> - nothing is pre-generated.</p>
<p>
  <button onclick="playHLS()">Play HLS (hls.js)</button>
  <button onclick="playDASH()">Play DASH (dash.js)</button>
</p>
<video id="v" controls></video>
<p id="status"></p>

<script src="https://cdn.jsdelivr.net/npm/hls.js@1"></script>
<script src="https://cdn.dashjs.org/latest/dash.all.min.js"></script>
<script>
  const v = document.getElementById('v'), status = document.getElementById('status')
  let hls, dash
  function reset() {
    if (hls) { hls.destroy(); hls = null }
    if (dash) { dash.reset(); dash = null }
    v.removeAttribute('src')
  }
  function playHLS() {
    reset()
    const url = '/hls/master.m3u8'
    if (window.Hls && Hls.isSupported()) {
      hls = new Hls(); hls.loadSource(url); hls.attachMedia(v); v.play()
      status.textContent = 'HLS via hls.js'
    } else if (v.canPlayType('application/vnd.apple.mpegurl')) {
      v.src = url; v.play(); status.textContent = 'HLS native (Safari)'
    } else status.textContent = 'HLS not supported here'
  }
  function playDASH() {
    reset()
    dash = dashjs.MediaPlayer().create()
    dash.initialize(v, '/hls/manifest.mpd', true)
    status.textContent = 'DASH via dash.js'
  }
</script>
`
