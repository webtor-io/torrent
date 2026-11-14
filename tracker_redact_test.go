package torrent

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anacrolix/torrent/internal/testutil"
)

// A fake private-tracker key, carried in the path and the query of the announce URL.
const testTrackerKey = "0123456789abcdef0123456789abcdef"

// Collects what both of the client's loggers write, Debug included.
type logCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) Handle(r log.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.WriteString(r.Msg.String())
	c.buf.WriteByte('\n')
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *logCapture) configure(cfg *ClientConfig) {
	cfg.Slogger = slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.Level(math.MinInt)}))
	l := log.NewLogger()
	l.SetHandlers(c)
	cfg.Logger = l.WithFilterLevel(log.Debug)
}

// Closes the connection without replying: net/http fails the announce with an error that quotes the
// request URL, and a websocket dial fails.
func hangUpTracker(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
			c.Close()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newLoggedTrackerClient(t *testing.T, logs *logCapture) *Client {
	cfg := TestingConfig(t)
	cfg.DisableTrackers = false
	logs.configure(cfg)
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { cl.Close() })
	return cl
}

func TestTrackerAnnounceLogsOmitTrackerKey(t *testing.T) {
	srv := hangUpTracker(t)
	var logs logCapture
	cl := newLoggedTrackerClient(t, &logs)
	mi := testutil.GreetingMetaInfo()
	mi.AnnounceList = [][]string{{
		srv.URL + "/" + testTrackerKey + "/announce?passkey=" + testTrackerKey,
		// url.Parse refuses this one, and its error quotes the whole string.
		"http://127.0.0.1:1/" + testTrackerKey + "/announce?passkey=" + testTrackerKey + "\x7f",
	}}
	tt, err := cl.AddTorrent(mi)
	require.NoError(t, err)
	var status, lastErr string
	require.Eventually(t, func() bool {
		cl.rLock()
		defer cl.rUnlock()
		for _, a := range tt.trackerAnnouncers {
			if ts, ok := a.(*trackerScraper); ok && ts.lastAnnounce.Err != nil {
				status, lastErr = ts.statusLine(), ts.lastAnnounce.Err.Error()
				return true
			}
		}
		return false
	}, 10*time.Second, 10*time.Millisecond)
	out := logs.String()
	// The status page lists each announcer by its URL; operators read it on private torrents too.
	var clientStatus strings.Builder
	cl.WriteStatus(&clientStatus)
	// The lines this test guards were written, and still name the tracker.
	require.Contains(t, out, "announce failed")
	require.Contains(t, out, "error parsing tracker url")
	assert.Contains(t, out, "urlKey="+srv.URL+" ")
	// The announcer's own row, not the error quoting the request: the tracker stays identifiable.
	assert.Contains(t, clientStatus.String(), "\n    \""+srv.URL+"\" ")
	for _, s := range []string{out, status, lastErr, clientStatus.String()} {
		assert.NotContains(t, s, testTrackerKey)
		assert.NotContains(t, s, "/announce")
	}
}

func TestWebsocketTrackerLogsOmitTrackerKey(t *testing.T) {
	srv := hangUpTracker(t)
	var logs logCapture
	cl := newLoggedTrackerClient(t, &logs)
	wsUrl := "ws" + strings.TrimPrefix(srv.URL, "http")
	mi := testutil.GreetingMetaInfo()
	mi.AnnounceList = [][]string{{wsUrl + "/" + testTrackerKey + "/announce?passkey=" + testTrackerKey}}
	tt, err := cl.AddTorrent(mi)
	require.NoError(t, err)
	// The tracker client prefixes its own lines with its URL.
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "websocket instance ended")
	}, 10*time.Second, 10*time.Millisecond)
	// The initial announce waits for a connection, and fails once dropping the torrent closes the
	// tracker client.
	tt.Drop()
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "error doing initial announce to")
	}, 10*time.Second, 10*time.Millisecond)
	out := logs.String()
	assert.Contains(t, out, `tracker client for "`+wsUrl+`"`)
	assert.Contains(t, out, `error doing initial announce to "`+wsUrl+`"`)
	assert.NotContains(t, out, testTrackerKey)
	assert.NotContains(t, out, "/announce")
}

func TestMetainfoSourceLogsOmitTrackerKey(t *testing.T) {
	srv := hangUpTracker(t)
	var logs logCapture
	cl := newLoggedTrackerClient(t, &logs)
	// A private tracker's .torrent download link carries the account's key.
	source := srv.URL + "/download.php?id=1&passkey=" + testTrackerKey
	_, err := cl.AddMagnet("magnet:?xt=urn:btih:" + testutil.GreetingMetaInfo().HashInfoBytes().HexString() +
		"&xs=" + url.QueryEscape(source))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "error using torrent source")
	}, 10*time.Second, 10*time.Millisecond)
	_, err = getTorrentSource(context.Background(), source, http.DefaultClient)
	require.Error(t, err)
	// url.Parse refuses this one, and its error quotes the whole string.
	_, parseErr := getTorrentSource(context.Background(), source+"\x7f", http.DefaultClient)
	require.Error(t, parseErr)
	out := logs.String()
	// The source is still named, by scheme and host.
	assert.Contains(t, out, "source="+srv.URL+" ")
	for _, s := range []string{out, err.Error(), parseErr.Error()} {
		assert.NotContains(t, s, testTrackerKey)
		assert.NotContains(t, s, "download.php")
	}
}
