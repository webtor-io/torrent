package torrent

import (
	"net"
	"testing"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/log"
	"github.com/anacrolix/missinggo/v2/pubsub"
	"github.com/go-quicktest/qt"

	"github.com/anacrolix/torrent/internal/testutil"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/stretchr/testify/require"
)

// TestRefreshCompletionFromStorage pins both halves of the contract that
// RefreshCompletionFromStorage exists for.
//
// The precondition is the surprising half, and it is asserted deliberately
// rather than assumed: a storage backend that marks a piece incomplete on its
// own initiative — a cache evicting to stay in budget — does NOT thereby
// change what this client believes. Reads answer from the in-memory
// _completedPieces bitmap, which only the client writes. A backend that
// discards data and updates only its completion store leaves the client
// serving whatever now lives at those offsets; for a hole-punched file that
// is zeroes, returned without error.
//
// If the first require below ever starts failing, the client has grown its
// own storage re-read and this method is redundant. That is a fine outcome —
// but it should be discovered by a red test, not assumed.
func TestRefreshCompletionFromStorage(t *testing.T) {
	cl := newTestingClient(t)
	td := t.TempDir()
	cs := storage.NewFile(td)
	defer cs.Close()
	tt := cl.newTorrent(metainfo.Hash{1}, cs)
	mi := testutil.GreetingMetaInfo()
	require.NoError(t, tt.SetInfoBytes(mi.InfoBytes))

	p := tt.Piece(0)

	// Bring the piece to "complete" the way the client itself would.
	cl.lock()
	tt.setPieceCompletion(0, g.Some(true))
	complete := tt.pieceComplete(0)
	cl.unlock()
	require.True(t, complete, "setup: piece should read as complete")

	// Now the backend drops the data and records that, without telling us.
	require.NoError(t, p.Storage().MarkNotComplete())

	cl.lock()
	stale := tt.pieceComplete(0)
	cl.unlock()
	require.True(t, stale,
		"precondition: the completion bitmap must NOT follow storage on its own — "+
			"if it did, an evicting backend would need no notification at all")

	// The whole point: correct the bitmap without re-hashing anything.
	changed := p.RefreshCompletionFromStorage()
	require.True(t, changed, "refresh should report the completion changed")

	cl.lock()
	after := tt.pieceComplete(0)
	cl.unlock()
	require.False(t, after, "after refresh the piece must read as incomplete")

	// Idempotent: a second refresh changes nothing and must not report that
	// it did, since callers may use the bool to decide whether to act.
	require.False(t, p.RefreshCompletionFromStorage(),
		"a second refresh should be a no-op")
}

// A torrent of numPieces one-chunk pieces, all wanted, over storage that reports the first
// sc.completed pieces complete. cfg is TestingConfig's if nil.
func newRefreshCompletionsTestTorrent(tb testing.TB, numPieces int, cfg *ClientConfig) (*Torrent, *storageClient) {
	if cfg == nil {
		cfg = TestingConfig(tb)
	}
	// The benchmark's peer has every piece; keep it when we do too.
	cfg.DropMutuallyCompletePeers = false
	cl, err := NewClient(cfg)
	qt.Assert(tb, qt.IsNil(err))
	tb.Cleanup(func() { cl.Close() })
	sc := &storageClient{}
	tor, _ := cl.AddTorrentOpt(AddTorrentOpts{
		InfoHash: testingTorrentInfoHash,
		Storage:  sc,
	})
	qt.Assert(tb, qt.IsNil(tor.setInfoUnlocked(&metainfo.Info{
		Pieces:      make([]byte, numPieces*metainfo.HashSize),
		PieceLength: defaultChunkSize,
		Length:      defaultChunkSize * int64(numPieces),
	})))
	tor.DownloadAll()
	return tor, sc
}

func allPieces(tor *Torrent) (ret []int) {
	for i := range tor.NumPieces() {
		ret = append(ret, i)
	}
	return
}

// Returns how many PieceStateChanges each piece got. Everything published so far is already
// queued on the subscription, so a sentinel published after it marks the end.
func drainPieceStateChanges(tor *Torrent, psc *pubsub.Subscription[PieceStateChange]) map[int]int {
	tor.pieceStateChanges.Publish(PieceStateChange{Index: -1})
	counts := make(map[int]int)
	for v := range psc.Values {
		if v.Index == -1 {
			break
		}
		counts[v.Index]++
	}
	return counts
}

func TestRefreshCompletionsFromStorage(t *testing.T) {
	var logs logCapture
	cfg := TestingConfig(t)
	logs.configure(cfg)
	tor, sc := newRefreshCompletionsTestTorrent(t, 8, cfg)
	pieces := allPieces(tor)
	wantPeers := func() bool {
		tor.cl.rLock()
		defer tor.cl.rUnlock()
		return tor.wantPeersEvent.IsSet()
	}
	qt.Assert(t, qt.IsTrue(wantPeers()), qt.Commentf("setup: a torrent that needs data wants peers"))
	psc := tor.SubscribePieceStateChanges()
	defer psc.Close()

	sc.completed = 5
	qt.Check(t, qt.Equals(tor.RefreshCompletionsFromStorage(pieces), 5))
	qt.Check(t, qt.DeepEquals(drainPieceStateChanges(tor, psc), map[int]int{0: 1, 1: 1, 2: 1, 3: 1, 4: 1}),
		qt.Commentf("one event per changed piece, none for unchanged ones"))
	qt.Check(t, qt.IsFalse(tor.Complete().Bool()))
	qt.Check(t, qt.IsTrue(wantPeers()))

	sc.completed = 8
	qt.Check(t, qt.Equals(tor.RefreshCompletionsFromStorage(pieces), 3))
	qt.Check(t, qt.DeepEquals(drainPieceStateChanges(tor, psc), map[int]int{5: 1, 6: 1, 7: 1}))
	qt.Check(t, qt.IsTrue(tor.Complete().Bool()), qt.Commentf("completing the last pieces completes the torrent"))
	qt.Check(t, qt.IsFalse(wantPeers()),
		qt.Commentf("a complete torrent that isn't seeding wants no peers, as after per-piece refreshes"))
	// Per piece, a completed piece leaves the request order before pending pieces, and the new
	// conns opened in between see the two disagree. The batch opens them once, at the end.
	qt.Check(t, qt.Not(qt.StringContains(logs.String(), "piece request order has")))

	qt.Check(t, qt.Equals(tor.RefreshCompletionsFromStorage(pieces), 0))
	qt.Check(t, qt.HasLen(drainPieceStateChanges(tor, psc), 0), qt.Commentf("nothing changed, nothing published"))

	qt.Check(t, qt.Equals(tor.RefreshCompletionsFromStorage(nil), 0))

	// After a batch, even one with nothing to do, the per-piece path still updates peer wanting
	// on its own.
	sc.completed = 7
	qt.Check(t, qt.IsTrue(tor.Piece(7).RefreshCompletionFromStorage()))
	qt.Check(t, qt.IsFalse(tor.Complete().Bool()))
	qt.Check(t, qt.IsTrue(wantPeers()))
}

func TestRefreshCompletionsFromStorageClosedTorrent(t *testing.T) {
	tor, sc := newRefreshCompletionsTestTorrent(t, 8, nil)
	tor.Drop()
	sc.completed = 8
	qt.Assert(t, qt.Equals(tor.RefreshCompletionsFromStorage(allPieces(tor)), 0))
	tor.cl.rLock()
	defer tor.cl.rUnlock()
	qt.Assert(t, qt.IsFalse(tor.haveAnyPieces()))
}

// Refreshes N pieces that storage now reports complete, either one Piece.RefreshCompletionFromStorage
// call per piece or one batch. A peer that has every piece and unchokes us holds requests for the
// torrent while its pieces are missing.
func BenchmarkRefreshCompletionsFromStorage(b *testing.B) {
	const numPieces = 20_000
	for _, bc := range []struct {
		name    string
		refresh func(*Torrent, []int)
	}{
		{"Single", func(tor *Torrent, pieces []int) {
			for _, i := range pieces {
				tor.Piece(i).RefreshCompletionFromStorage()
			}
		}},
		{"Batch", func(tor *Torrent, pieces []int) {
			tor.RefreshCompletionsFromStorage(pieces)
		}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			cfg := TestingConfig(b)
			// Single logs a request order warning per piece. Measure the refresh, not the log sink.
			cfg.Logger.SetHandlers(log.DiscardHandler)
			tor, sc := newRefreshCompletionsTestTorrent(b, numPieces, cfg)
			pieces := allPieces(tor)
			cl := tor.cl
			cl.lock()
			c := cl.newConnection(nil, newConnectionOpts{
				network:    "tcp",
				remoteAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5678},
			})
			c.setTorrent(tor)
			c.initMessageWriter()
			err := tor.addPeerConn(c)
			c.onPeerHasAllPieces()
			c.peerChoking = false
			cl.unlock()
			qt.Assert(b, qt.IsNil(err))
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				sc.completed = 0
				tor.RefreshCompletionsFromStorage(pieces)
				cl.lock()
				// What the conn's writer would do: flush, then request.
				c.messageWriter.mu.Lock()
				c.messageWriter.writeBuffer.Reset()
				c.messageWriter.mu.Unlock()
				c.updateRequestsWithReason("benchmark")
				requests := c.requestState.Requests.GetCardinality()
				cl.unlock()
				qt.Assert(b, qt.Not(qt.Equals(requests, 0)))
				sc.completed = numPieces
				b.StartTimer()
				bc.refresh(tor, pieces)
			}
			cl.lock()
			haveAll := tor.haveAllPieces()
			cl.unlock()
			qt.Assert(b, qt.IsTrue(haveAll))
		})
	}
}
