package torrent

import (
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func v2Info() *metainfo.Info {
	// Two files, piece-aligned: a at 0 (10 bytes, then 6 bytes of padding),
	// b at 16 (20 bytes) — the data ends at 36, not at 10+20.
	return &metainfo.Info{
		Name:        "v2",
		PieceLength: 16,
		MetaVersion: 2,
		FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
			"a": {File: metainfo.FileTreeFile{Length: 10}},
			"b": {File: metainfo.FileTreeFile{Length: 20}},
		}},
	}
}

func TestCacheLength_V2EndsAtLastFile(t *testing.T) {
	tor := &Torrent{info: v2Info(), chunkSize: 4}
	tor.cacheLength()
	if got := tor.length(); got != 36 {
		t.Fatalf("length = %d, want 36 (end of the last piece-aligned file)", got)
	}
	// The tail of the last file is inside the torrent.
	begin, end := tor.byteRegionPieces(31, 1)
	if begin != 1 || end != 2 {
		t.Fatalf("byteRegionPieces(31,1) = [%d,%d), want [1,2)", begin, end)
	}
	req, ok := tor.offsetRequest(35)
	if !ok || req.Index != 2 {
		t.Fatalf("offsetRequest(35) = %+v ok=%v, want piece 2", req, ok)
	}
	// The length agrees with the piece count the metainfo reports.
	if n := int((tor.length() + tor.info.PieceLength - 1) / tor.info.PieceLength); n != tor.info.NumPieces() {
		t.Fatalf("pieces covered by length = %d, NumPieces = %d", n, tor.info.NumPieces())
	}
}

func TestCacheLength_V1IsTheSum(t *testing.T) {
	info := &metainfo.Info{
		Name:        "v1",
		PieceLength: 16,
		Pieces:      make([]byte, 20*2),
		Files: []metainfo.FileInfo{
			{Path: []string{"a"}, Length: 10},
			{Path: []string{"b"}, Length: 20},
		},
	}
	tor := &Torrent{info: info, chunkSize: 4}
	tor.cacheLength()
	if got := tor.length(); got != 30 {
		t.Fatalf("length = %d, want 30", got)
	}
}
