package commands

import (
	"net/http"
	"testing"
)

// TestServersBoundSlowClients: the serve commands ran an http.Server with no timeout at all, so a client could hold
// a connection, a source handle and a segment's budget open by sending or reading nothing. A header must now arrive
// within a bound, an idle connection closes, and an HLS response must be written within its own bound.
func TestServersBoundSlowClients(t *testing.T) {
	srv := newServer(":0", http.NotFoundHandler(), segmentWriteTimeout)
	if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 || srv.WriteTimeout != segmentWriteTimeout {
		t.Errorf("header %v, idle %v, write %v; want all bounded", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.WriteTimeout)
	}
	if direct := newServer(":0", http.NotFoundHandler(), 0); direct.WriteTimeout != 0 || direct.ReadHeaderTimeout <= 0 {
		t.Errorf("direct play: write %v, header %v; want no write deadline on a whole film, a header bound still", direct.WriteTimeout, direct.ReadHeaderTimeout)
	}
}
