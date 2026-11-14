package torrent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/missinggo/v2/panicif"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/tracker/shared"
)

// Add HTTP endpoints that serve the metainfo. They will be used if the torrent info isn't obtained
// yet. The Client HTTP client is used.
func (t *Torrent) AddSources(sources []string) {
	for _, s := range sources {
		_, loaded := t.activeSources.LoadOrStore(s, nil)
		if loaded {
			continue
		}
		go t.sourcer(s)
	}
}

// Tries fetching metainfo from a (HTTP) source until no longer necessary.
func (t *Torrent) sourcer(source string) {
	var err error
	defer func() {
		panicif.False(t.activeSources.CompareAndSwap(source, nil, err))
	}()
	ctx := t.getInfoCtx
	for {
		var retry g.Option[time.Duration]
		retry, err = t.trySource(source)
		if err == nil || ctx.Err() != nil {
			return
		}
		// A private tracker's .torrent download link carries the account's key.
		t.slogger().Warn("error using torrent source", "source", shared.RedactUrl(source), "err", err)
		if !retry.Ok {
			return
		}
		select {
		case <-time.After(retry.Unwrap()):
		case <-ctx.Done():
		}
	}
}

// If retry is None, take the error you get as final.
func (t *Torrent) trySource(source string) (retry g.Option[time.Duration], err error) {
	t.sourceMutex.Lock()
	defer t.sourceMutex.Unlock()
	ctx := t.getInfoCtx
	if ctx.Err() != nil {
		return
	}
	var mi metainfo.MetaInfo
	mi, err = getTorrentSource(ctx, source, t.cl.config.MetainfoSourcesClient)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		retry.Set(time.Minute + time.Duration(rand.Int64N(int64(time.Minute))))
		return
	}
	err = t.cl.config.MetainfoSourcesMerger(t, &mi)
	return
}

// The largest info dicts seen in the wild are about 14 MiB (see maxMetadataSize, which caps them at
// 16 MiB when peers hand them over). A hybrid torrent adds v2 piece layers outside the info dict,
// 32 bytes for each 20-byte v1 piece hash, so at most 1.6 times as much again: about 42 MiB in all.
// magnet2torrent, which passes the metainfo on, caps its gRPC replies at 50 MiB, so nothing larger
// could leave it anyway. It bounds what a source can make us buffer.
const maxSourceBodyLen = 50 << 20

func getTorrentSource(ctx context.Context, source string, hc *http.Client) (mi metainfo.MetaInfo, err error) {
	// Both errors quote the source URL, and with it a private tracker's key.
	var req *http.Request
	if req, err = http.NewRequestWithContext(ctx, http.MethodGet, source, nil); err != nil {
		err = shared.RedactUrlError(err)
		return
	}
	var resp *http.Response
	if resp, err = hc.Do(req); err != nil {
		err = shared.RedactUrlError(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("unexpected response status code: %v", resp.StatusCode)
		return
	}
	// A magnet names its own sources, so whoever crafts one decides how much we read.
	var body []byte
	if body, err = io.ReadAll(io.LimitReader(resp.Body, maxSourceBodyLen+1)); err != nil {
		return
	}
	if len(body) > maxSourceBodyLen {
		err = fmt.Errorf("metainfo source response exceeds %d bytes", maxSourceBodyLen)
		return
	}
	// As before, bytes after the metainfo are ignored. No string is longer than the body, so a
	// declared length past it is refused before the decoder allocates it (default bound: 128 MiB).
	dec := bencode.NewDecoder(bytes.NewReader(body))
	dec.MaxStrLen = int64(len(body))
	if err = dec.Decode(&mi); err != nil {
		// The decoder quotes dict keys, which are the source's: it can echo our key, at any length.
		err = fmt.Errorf("error decoding metainfo source response (%d bytes): %s",
			len(body), shared.TruncateTrackerText(shared.RedactEcho(err.Error(), req.URL)))
	}
	return
}
