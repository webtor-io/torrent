package torrent

import (
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// Redial replaces a live connection with a new one to the same peer, and
// PeerStats reports whether that peer is choking us.
func TestPeerConnRedial(t *testing.T) {
	seederDir := t.TempDir()
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seederDir, "payload.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(filepath.Join(seederDir, "payload.bin")); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}

	// 32 KiB/s keeps the 1 MiB transfer, and so the connection, alive.
	cfg := TestingConfig(t)
	cfg.Seed = true
	cfg.DataDir = seederDir
	// One transport, so the peer has one address and one connection.
	cfg.DisableUTP = true
	cfg.UploadRateLimiter = rate.NewLimiter(32<<10, 32<<10)
	// TestingConfig allows 5 bytes of pending request data per connection,
	// meant for 2-byte chunks; this payload uses 16 KiB ones.
	cfg.MaxAllocPeerRequestDataPerConn = NewDefaultClientConfig().MaxAllocPeerRequestDataPerConn
	seeder, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer seeder.Close()
	st, err := seeder.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	st.VerifyData()

	cfg = TestingConfig(t)
	cfg.DataDir = t.TempDir()
	cfg.DisableUTP = true
	leecher, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer leecher.Close()
	lt, err := leecher.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	r := lt.NewReader()
	defer r.Close()
	go func() { _, _ = io.Copy(io.Discard, r) }()
	lt.AddClientPeer(seeder)

	var first *PeerConn
	waitFor(t, "an unchoked connection", func() bool {
		for _, pc := range lt.PeerConns() {
			if s := pc.Stats(); !s.PeerChoking && s.BytesReadData.Int64() > 0 {
				first = pc
				return true
			}
		}
		return false
	})
	if s := first.Stats(); !s.PeerChokingSince.IsZero() {
		t.Errorf("PeerChokingSince = %v while unchoked, want zero", s.PeerChokingSince)
	}

	before := map[*PeerConn]bool{}
	for _, pc := range lt.PeerConns() {
		before[pc] = true
	}
	first.Redial()
	waitFor(t, "a connection opened by the redial", func() bool {
		for _, pc := range lt.PeerConns() {
			if !before[pc] {
				return true
			}
		}
		return false
	})
	for _, pc := range lt.PeerConns() {
		if pc == first {
			t.Error("the redialled connection is still listed")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s within 10s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
