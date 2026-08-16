package torrent

import (
	"testing"

	g "github.com/anacrolix/generics"
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
