package httpTracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/tracker/shared"
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

func TestAnnounceRefusalOmitsBody(t *testing.T) {
	// Tracker error pages often echo the request.
	body := "passkey " + testKey + " is not registered: GET /" + testKey + "/announce"
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(body))
	})
	_, err := privateTrackerClient(t, srv).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	require.Error(t, err)
	assertNoTrackerUrl(t, err.Error())
	assert.Equal(t, fmt.Sprintf("response from tracker: 403 Forbidden: %d bytes", len(body)), err.Error())
}

func TestAnnounceRefusalOmitsReasonPhrase(t *testing.T) {
	// The reason phrase of the status line is the tracker's own text, up to the size limit of the
	// response header.
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		c, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		buf.WriteString("HTTP/1.1 403 passkey " + testKey + " \x1b[2J\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		buf.Flush()
	})
	_, err := privateTrackerClient(t, srv).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	require.Error(t, err)
	assertNoTrackerUrl(t, err.Error())
	assert.NotContains(t, err.Error(), "\x1b")
	assert.Equal(t, "response from tracker: 403 Forbidden: 0 bytes", err.Error())
}

func TestAnnounceUndecodableReplyOmitsBody(t *testing.T) {
	body := "<html>passkey " + testKey + " is not registered</html>"
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})
	_, err := privateTrackerClient(t, srv).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	require.Error(t, err)
	assertNoTrackerUrl(t, err.Error())
	assert.NotContains(t, err.Error(), "html")
	assert.Contains(t, err.Error(), fmt.Sprintf("error decoding response from tracker (%d bytes)", len(body)))
}

func TestAnnounceDecodeErrorIsTruncated(t *testing.T) {
	// The decoder quotes a dict key it cannot pair with a value, and the key is the tracker's.
	key := strings.Repeat("x", 10_000) + testKey
	body := fmt.Sprintf("d%d:%se", len(key), key)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})
	_, err := privateTrackerClient(t, srv).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testKey)
	prefix := fmt.Sprintf("error decoding response from tracker (%d bytes): ", len(body))
	assert.True(t, strings.HasPrefix(err.Error(), prefix), "%v", err)
	assert.LessOrEqual(t, len(err.Error()), len(prefix+"...")+shared.MaxTrackerTextLen)
}

// A valid compact reply whose peers string makes it bodyLen bytes or a little more.
func compactReplyOfLen(bodyLen int) []byte {
	peersLen := bodyLen / 6 * 6
	return []byte(fmt.Sprintf("d8:intervali1800e5:peers%d:%se", peersLen, strings.Repeat("\x00", peersLen)))
}

func TestAnnounceReplyBodyIsCapped(t *testing.T) {
	var reply []byte
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(reply)
	})
	cl := privateTrackerClient(t, srv)

	// A reply with thousands of peers is well inside the cap.
	reply = compactReplyOfLen(6 * 10_000)
	res, err := cl.Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	require.NoError(t, err)
	assert.Len(t, res.Peers, 10_000)

	// The same reply past the cap is refused instead of read whole.
	reply = compactReplyOfLen(maxResponseBodyLen + 6)
	require.Greater(t, len(reply), maxResponseBodyLen)
	_, err = cl.Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	require.Error(t, err)
	assert.Equal(t, fmt.Sprintf("response from tracker: 200 OK: body exceeds %d bytes", maxResponseBodyLen), err.Error())
}

func TestAnnounceStopsReadingAtCap(t *testing.T) {
	const offered = 64 << 20
	written := make(chan int, 1)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 64<<10)
		total := 0
		for total < offered {
			n, err := w.Write(chunk)
			total += n
			if err != nil {
				break
			}
		}
		written <- total
	})
	_, err := privateTrackerClient(t, srv).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	require.Error(t, err)
	// The client hangs up after the cap. What the server still got to write went into socket
	// buffers, a few MiB at most, not into our memory.
	assert.Less(t, <-written, offered/2)
}

func TestAnnounceFailureReasonIsTruncated(t *testing.T) {
	var reason string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, err := bencode.Marshal(map[string]string{"failure reason": reason})
		if err != nil {
			panic(err)
		}
		w.Write(b)
	})
	cl := privateTrackerClient(t, srv)

	// Operators classify refusals by this text, so a usual one is kept whole.
	reason = "Unregistered torrent"
	_, err := cl.Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	assert.EqualError(t, err, `tracker gave failure reason: "Unregistered torrent"`)

	reason = "Unregistered torrent " + strings.Repeat("x", 10_000) + testKey
	_, err = cl.Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testKey)
	assert.True(t, strings.HasPrefix(err.Error(), `tracker gave failure reason: "Unregistered torrent xxx`), "%v", err)
	assert.LessOrEqual(t, len(err.Error()), len(`tracker gave failure reason: ""...`)+shared.MaxTrackerTextLen)
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

func TestAnnounceFailureReasonRedactsEchoedKey(t *testing.T) {
	var reason string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, err := bencode.Marshal(map[string]string{"failure reason": reason})
		if err != nil {
			panic(err)
		}
		w.Write(b)
	})
	announce := func(rawUrl string) error {
		u, err := url.Parse(rawUrl)
		require.NoError(t, err)
		_, err = NewClient(u, NewClientOpts{}).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
		require.Error(t, err)
		return err
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	// Trackers quote a refused key back, and truncation alone keeps a short reason whole.
	reason = "Invalid passkey (33 - " + testKey + ")"
	for _, rawUrl := range []string{
		srv.URL + "/" + testKey + "/announce",
		srv.URL + "/announce?passkey=" + testKey,
		"http://u:" + testKey + "@" + host + "/announce",
	} {
		assert.EqualError(t, announce(rawUrl), `tracker gave failure reason: "Invalid passkey (33 - <redacted>)"`, rawUrl)
	}

	// Redacted before the cut: a key the cut ran through would leave its first bytes.
	reason = strings.Repeat("x", shared.MaxTrackerTextLen-6) + testKey + strings.Repeat("x", 1000)
	err := announce(srv.URL + "/announce?passkey=" + testKey)
	assert.NotContains(t, err.Error(), testKey[:6])
	assert.LessOrEqual(t, len(err.Error()), len(`tracker gave failure reason: ""...`)+shared.MaxTrackerTextLen)

	// Short query values are left alone.
	reason = "id 1 is not registered"
	assert.EqualError(t, announce(srv.URL+"/announce?id=1"), `tracker gave failure reason: "id 1 is not registered"`)
}

func TestAnnounceDecodeErrorRedactsEchoedKey(t *testing.T) {
	// The decoder quotes a dict key it cannot pair with a value.
	key := "passkey " + testKey
	body := fmt.Sprintf("d%d:%se", len(key), key)
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})
	_, err := privateTrackerClient(t, srv).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testKey)
	assert.Contains(t, err.Error(), "passkey <redacted>")
}
