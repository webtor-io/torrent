package torrent

import (
	"strconv"
	"testing"

	"github.com/go-quicktest/qt"

	"github.com/anacrolix/torrent/metainfo"
)

// Info.NumPieces walks the whole file tree of a v2 torrent, and the piece count
// is asked for on hot paths: onSetInfo asks once per piece per connected peer
// (peerHasPiece → peerHasAllPieces), every read asks through byteRegionPieces.
// With the walk behind numPieces, setting the info of a v2 torrent with tens of
// thousands of files held the client lock for O(pieces×peers×files):
// magnet2torrent, 2026-10-12, an 11 MB info dict kept its clients busy for
// hours and the process OOMed.
func v2InfoForTesting(files int) *metainfo.Info {
	dir := make(map[string]metainfo.FileTree, files)
	for i := range files {
		dir[strconv.Itoa(i)] = metainfo.FileTree{File: metainfo.FileTreeFile{Length: 1, PiecesRoot: string(make([]byte, 32))}}
	}
	return &metainfo.Info{
		Name:        "v2",
		MetaVersion: 2,
		PieceLength: 1 << 14,
		FileTree:    metainfo.FileTree{Dir: map[string]metainfo.FileTree{"d": {Dir: dir}}},
	}
}

func TestNumPiecesV2DoesNotWalkFileTree(t *testing.T) {
	tt := newTestingClient(t).newTorrentForTesting()
	qt.Assert(t, qt.IsNil(tt.setInfoUnlocked(v2InfoForTesting(1000))))
	qt.Assert(t, qt.Equals(tt.numPieces(), 1000))
	qt.Check(t, qt.Equals(testing.AllocsPerRun(10, func() { tt.numPieces() }), 0))
	qt.Check(t, qt.Equals(testing.AllocsPerRun(10, func() { tt.byteRegionPieces(0, 1) }), 0))
}
