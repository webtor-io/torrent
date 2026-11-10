package httpTracker

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anacrolix/torrent/types/infohash"
)

// A fake private-tracker key. Private trackers put it in the path or the query of the announce
// URL, and it must not reach logs or error strings.
const testKey = "0123456789abcdef0123456789abcdef"

// Returns a client for an announce URL on srv that carries testKey both ways.
func privateTrackerClient(t *testing.T, srv *httptest.Server) Client {
	u, err := url.Parse(srv.URL + "/" + testKey + "/announce?passkey=" + testKey)
	require.NoError(t, err)
	return NewClient(u, NewClientOpts{})
}

// Fails if s quotes the announce URL past its host: the key, the path, or the announce parameters
// we append.
func assertNoTrackerUrl(t *testing.T, s string) {
	t.Helper()
	for _, part := range []string{testKey, "/announce", "/scrape", "info_hash", "peer_id"} {
		assert.NotContains(t, s, part)
	}
}

// Closes the connection without replying, so net/http fails the request with a *url.Error.
func hangUp(w http.ResponseWriter, r *http.Request) {
	c, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		c.Close()
	}
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestAnnounceTransportErrorOmitsTrackerUrl(t *testing.T) {
	srv := serve(t, hangUp)
	_, err := privateTrackerClient(t, srv).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	require.Error(t, err)
	assertNoTrackerUrl(t, err.Error())
	// The tracker is still named, by scheme and host.
	assert.Contains(t, err.Error(), srv.URL)
}

func TestAnnounceCancelledKeepsCause(t *testing.T) {
	srv := serve(t, hangUp)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := privateTrackerClient(t, srv).Announce(ctx, AnnounceRequest{}, AnnounceOpt{})
	require.Error(t, err)
	assertNoTrackerUrl(t, err.Error())
	// The announcer logs a cancelled announce at a lower level, and tells it by its cause.
	assert.True(t, errors.Is(err, context.Canceled), "%v", err)
	var ue *url.Error
	assert.True(t, errors.As(err, &ue), "%v", err)
}

func TestScrapeOmitsTrackerUrl(t *testing.T) {
	srv := serve(t, hangUp)
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	_, err := privateTrackerClient(t, srv).Scrape(context.Background(), []infohash.T{{}})
	require.Error(t, err)
	assertNoTrackerUrl(t, err.Error())
	assertNoTrackerUrl(t, logged.String())
	assert.Contains(t, logged.String(), srv.URL)
}
