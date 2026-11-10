package torrent

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anacrolix/torrent/tracker/shared"
)

// Serves a metainfo of n bytes: a dict with one key the decoder discards, padded by its value.
// Returns the source URL and how many bytes the server got to write before the reader hung up.
func paddedMetainfoSource(t *testing.T, n int) (string, <-chan int) {
	l := n - len("d3:pad:e")
	for len("d3:pad:e")+len(strconv.Itoa(l))+l > n {
		l--
	}
	head := fmt.Sprintf("d3:pad%d:", l)
	require.Equal(t, n, len(head)+l+len("e"))
	written := make(chan int, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		total := 0
		defer func() { written <- total }()
		// Chunked, so the server holds none of it.
		chunk := bytes.Repeat([]byte("x"), 1<<16)
		write := func(b []byte) bool {
			m, err := w.Write(b)
			total += m
			return err == nil
		}
		if !write([]byte(head)) {
			return
		}
		for left := l; left > 0; left -= len(chunk) {
			if !write(chunk[:min(left, len(chunk))]) {
				return
			}
		}
		write([]byte("e"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/a.torrent", written
}

func TestMetainfoSourceBodyLimit(t *testing.T) {
	ctx := context.Background()
	source, _ := paddedMetainfoSource(t, maxSourceBodyLen)
	_, err := getTorrentSource(ctx, source, http.DefaultClient)
	require.NoError(t, err)

	tooLong := fmt.Sprintf("metainfo source response exceeds %d bytes", maxSourceBodyLen)
	source, _ = paddedMetainfoSource(t, maxSourceBodyLen+1)
	_, err = getTorrentSource(ctx, source, http.DefaultClient)
	assert.EqualError(t, err, tooLong)

	// Refused without reading it all: what we left unread fills socket buffers, a few MiB at most.
	source, written := paddedMetainfoSource(t, 2*maxSourceBodyLen)
	_, err = getTorrentSource(ctx, source, http.DefaultClient)
	assert.EqualError(t, err, tooLong)
	assert.Less(t, <-written, maxSourceBodyLen*3/2)
}

func TestMetainfoSourceDecodeErrorRedactsEchoedKey(t *testing.T) {
	// The decoder quotes a dict key it cannot pair with a value, as long as the source sends it.
	key := "passkey " + testTrackerKey + " " + strings.Repeat("x", 4*shared.MaxTrackerTextLen)
	body := fmt.Sprintf("d%d:%se", len(key), key)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	_, err := getTorrentSource(context.Background(), srv.URL+"/download.php?id=1&passkey="+testTrackerKey, http.DefaultClient)
	require.Error(t, err)
	prefix := fmt.Sprintf("error decoding metainfo source response (%d bytes): ", len(body))
	assert.True(t, strings.HasPrefix(err.Error(), prefix), "%v", err)
	assert.Contains(t, err.Error(), `key "passkey <redacted> xxx`)
	assert.NotContains(t, err.Error(), testTrackerKey)
	assert.LessOrEqual(t, len(err.Error()), len(prefix)+shared.MaxTrackerTextLen+len("..."))
}

func TestMetainfoSourceDeclaredStringLongerThanBodyIsRefused(t *testing.T) {
	// See TestAnnounceDeclaredStringLongerThanReplyIsRefused: the declared length is bounded by the body.
	body := "d4:infod6:pieces100000000:"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	_, err := getTorrentSource(context.Background(), srv.URL+"/x.torrent", http.DefaultClient)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("exceeds limit (%d)", len(body)))
}
