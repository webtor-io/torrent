package torrent

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/go-quicktest/qt"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/internal/testutil"
	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"
)

// The greeting torrent's info, flagged private (BEP 27) when private is set. Returns the info
// bytes, which also give the infohash.
func greetingInfoBytes(t testing.TB, private bool) []byte {
	info := testutil.Greeting.Info(5)
	if private {
		info.Private = new(bool)
		*info.Private = true
	}
	b, err := bencode.Marshal(info)
	qt.Assert(t, qt.IsNil(err))
	return b
}

// A DhtServer that records announces instead of touching the network. The torrent package only
// reaches the DHT for a torrent through Announce, which does both the get_peers traversal and the
// announce_peer step, so counting Announce counts both.
type recordingDhtServer struct {
	mu        sync.Mutex
	announces []*recordedDhtAnnounce
	started   chan *recordedDhtAnnounce
}

func newRecordingDhtServer() *recordingDhtServer {
	return &recordingDhtServer{started: make(chan *recordedDhtAnnounce, 100)}
}

type recordedDhtAnnounce struct {
	infoHash  [20]byte
	peers     chan dht.PeersValues
	closeOnce sync.Once
	// Closed when the torrent stops the announce.
	closed chan struct{}
}

func (me *recordedDhtAnnounce) Peers() <-chan dht.PeersValues {
	return me.peers
}

func (me *recordedDhtAnnounce) Close() {
	me.closeOnce.Do(func() {
		close(me.closed)
		close(me.peers)
	})
}

func (me *recordingDhtServer) Stats() interface{}          { return nil }
func (me *recordingDhtServer) ID() (id [20]byte)           { return }
func (me *recordingDhtServer) AddNode(krpc.NodeInfo) error { return nil }
func (me *recordingDhtServer) Ping(*net.UDPAddr)           {}
func (me *recordingDhtServer) WriteStatus(io.Writer)       {}
func (me *recordingDhtServer) Addr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}

func (me *recordingDhtServer) Announce(hash [20]byte, port int, impliedPort bool) (DhtAnnounce, error) {
	a := &recordedDhtAnnounce{
		infoHash: hash,
		peers:    make(chan dht.PeersValues),
		closed:   make(chan struct{}),
	}
	me.mu.Lock()
	me.announces = append(me.announces, a)
	me.mu.Unlock()
	me.started <- a
	return a, nil
}

func (me *recordingDhtServer) numAnnounces(ih [20]byte) (n int) {
	me.mu.Lock()
	defer me.mu.Unlock()
	for _, a := range me.announces {
		if a.infoHash == ih {
			n++
		}
	}
	return
}

// Waits for an announce of ih to start.
func (me *recordingDhtServer) waitAnnounce(t testing.TB, ih [20]byte) *recordedDhtAnnounce {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case a := <-me.started:
			if a.infoHash == ih {
				return a
			}
		case <-timeout:
			t.Fatalf("no DHT announce of %x", ih)
		}
	}
}

func newClientWithRecordingDht(t *testing.T) (*Client, *recordingDhtServer) {
	cl, err := NewClient(TestingConfig(t))
	qt.Assert(t, qt.IsNil(err))
	t.Cleanup(func() { cl.Close() })
	ds := newRecordingDhtServer()
	cl.AddDhtServer(ds)
	return cl, ds
}

// How long to keep watching for something that must not happen, once whatever would have caused it
// has had its chance.
const bep27QuietPeriod = 200 * time.Millisecond

// A private torrent added with its info is never announced to, or looked up in, the DHT. A public
// one added after it is, which shows the announcers had their chance.
func TestPrivateTorrentNotAnnouncedToDht(t *testing.T) {
	cl, ds := newClientWithRecordingDht(t)
	privateInfo := greetingInfoBytes(t, true)
	privateT, _ := cl.AddTorrentOpt(AddTorrentOpts{
		InfoHash:  metainfo.HashBytes(privateInfo),
		InfoBytes: privateInfo,
	})
	privateT.DownloadAll()
	publicInfo := greetingInfoBytes(t, false)
	publicT, _ := cl.AddTorrentOpt(AddTorrentOpts{
		InfoHash:  metainfo.HashBytes(publicInfo),
		InfoBytes: publicInfo,
	})
	publicT.DownloadAll()
	ds.waitAnnounce(t, publicT.InfoHash())
	time.Sleep(bep27QuietPeriod)
	qt.Check(t, qt.Equals(ds.numAnnounces(privateT.InfoHash()), 0))
}

// A magnet uses the DHT while its info is unknown, since that is how it finds peers to get the info
// from. Once the info arrives and says private, the running announce stops at once and no new one
// starts. A public magnet's announce runs on.
func TestMagnetDhtAnnounceOnceInfoArrives(t *testing.T) {
	for _, private := range []bool{false, true} {
		name := "public"
		if private {
			name = "private"
		}
		t.Run(name, func(t *testing.T) {
			cl, ds := newClientWithRecordingDht(t)
			infoBytes := greetingInfoBytes(t, private)
			ih := metainfo.HashBytes(infoBytes)
			tt, _ := cl.AddTorrentInfoHash(ih)
			ann := ds.waitAnnounce(t, ih)
			qt.Assert(t, qt.IsNil(tt.SetInfoBytes(infoBytes)))
			// Keep wanting conns, so only the private flag can keep a new announce from starting.
			tt.DownloadAll()
			if private {
				select {
				case <-ann.closed:
				case <-time.After(10 * time.Second):
					t.Fatal("DHT announce still running after the info said private")
				}
			} else {
				select {
				case <-ann.closed:
					t.Fatal("public torrent's DHT announce stopped when the info arrived")
				case <-time.After(bep27QuietPeriod):
				}
			}
			time.Sleep(bep27QuietPeriod)
			qt.Check(t, qt.Equals(ds.numAnnounces(ih), 1))
		})
	}
}

// Adds a conn to tt as if its peer had just connected, ready for an extended handshake. Client lock
// must be held.
func addBep27TestConn(t *testing.T, tt *Torrent) *PeerConn {
	return addBep27TestConnExtended(t, tt, true)
}

// Like addBep27TestConn. A peer without the extension protocol (extended false) never sends an
// extended handshake, so it is listed for PEX when it's added.
func addBep27TestConnExtended(t *testing.T, tt *Torrent, extended bool) *PeerConn {
	cl := tt.cl
	c := cl.newConnection(nil, newConnectionOpts{
		network:    "tcp",
		remoteAddr: &net.TCPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5678},
	})
	c.PeerExtensionBytes.SetBit(pp.ExtensionBitLtep, extended)
	c.setTorrent(tt)
	c.initMessageWriter()
	c.LocalLtepProtocolMap = &cl.defaultLocalLtepProtocolMap
	qt.Assert(t, qt.IsNil(tt.addPeerConn(c)))
	return c
}

var extendedHandshakeOfferingPex = bencode.MustMarshal(pp.ExtendedHandshakeMessage{
	M: map[pp.ExtensionName]pp.ExtensionNumber{pp.ExtensionNamePex: 1},
})

// With the info known to be private, a peer offering ut_pex in its extended handshake gets no PEX:
// the conn isn't listed for sharing with other peers, and PEX isn't enabled on it.
func TestPexNotStartedForPrivateTorrent(t *testing.T) {
	for _, private := range []bool{false, true} {
		name := "public"
		if private {
			name = "private"
		}
		t.Run(name, func(t *testing.T) {
			cl, err := NewClient(TestingConfig(t))
			qt.Assert(t, qt.IsNil(err))
			defer cl.Close()
			infoBytes := greetingInfoBytes(t, private)
			tt, _ := cl.AddTorrentOpt(AddTorrentOpts{
				InfoHash:  metainfo.HashBytes(infoBytes),
				InfoBytes: infoBytes,
			})
			// So the torrent wants conns.
			tt.DownloadAll()
			cl.lock()
			defer cl.unlock()
			c := addBep27TestConn(t, tt)
			qt.Assert(t, qt.IsNil(c.onReadExtendedMsg(pp.HandshakeExtendedID, extendedHandshakeOfferingPex)))
			qt.Check(t, qt.Equals(c.pex.IsEnabled(), !private))
			listed := 1
			if private {
				listed = 0
			}
			qt.Check(t, qt.Equals(tt.pex.nc, listed))
			qt.Check(t, qt.Equals(tt.pex.numPending(nil), listed))
		})
	}
}

// A private torrent's peer that doesn't speak the extension protocol isn't listed for PEX either.
func TestPrivateTorrentNonExtendedPeerNotListedForPex(t *testing.T) {
	for _, private := range []bool{false, true} {
		name := "public"
		if private {
			name = "private"
		}
		t.Run(name, func(t *testing.T) {
			cl, err := NewClient(TestingConfig(t))
			qt.Assert(t, qt.IsNil(err))
			defer cl.Close()
			infoBytes := greetingInfoBytes(t, private)
			tt, _ := cl.AddTorrentOpt(AddTorrentOpts{
				InfoHash:  metainfo.HashBytes(infoBytes),
				InfoBytes: infoBytes,
			})
			tt.DownloadAll()
			cl.lock()
			defer cl.unlock()
			addBep27TestConnExtended(t, tt, false)
			listed := 1
			if private {
				listed = 0
			}
			qt.Check(t, qt.Equals(tt.pex.nc, listed))
		})
	}
}

// Ported from upstream anacrolix/torrent 1014ccaf (network-bounds_test.go). PEX enabled on a
// magnet's conn before the info arrives must be stopped if the info turns out to be private.
func TestPexDisabledWhenInfoIsPrivate(t *testing.T) {
	cl, err := NewClient(TestingConfig(t))
	qt.Assert(t, qt.IsNil(err))
	defer cl.Close()
	infoBytes := greetingInfoBytes(t, true)
	tt, _ := cl.AddTorrentInfoHash(metainfo.HashBytes(infoBytes))
	cl.lock()
	defer cl.unlock()
	c := addBep27TestConn(t, tt)
	qt.Assert(t, qt.IsNil(c.onReadExtendedMsg(pp.HandshakeExtendedID, extendedHandshakeOfferingPex)))
	qt.Assert(t, qt.IsTrue(c.pex.IsEnabled()))
	qt.Assert(t, qt.IsNil(tt.setInfoBytesLocked(infoBytes)))
	qt.Check(t, qt.IsFalse(c.pex.IsEnabled()))
	// A later handshake doesn't turn it back on.
	qt.Assert(t, qt.IsNil(c.onReadExtendedMsg(pp.HandshakeExtendedID, extendedHandshakeOfferingPex)))
	qt.Check(t, qt.IsFalse(c.pex.IsEnabled()))
}

// What each side of a real connection offers in its extended handshake: a private torrent's conns
// don't offer ut_pex, so a well-behaved peer won't send PEX for it either.
func TestPrivateTorrentExtendedHandshakeOmitsPex(t *testing.T) {
	for _, private := range []bool{false, true} {
		name := "public"
		if private {
			name = "private"
		}
		t.Run(name, func(t *testing.T) {
			infoBytes := greetingInfoBytes(t, private)
			opts := AddTorrentOpts{
				InfoHash:  metainfo.HashBytes(infoBytes),
				InfoBytes: infoBytes,
			}
			newClient := func() (*Client, <-chan map[pp.ExtensionName]pp.ExtensionNumber) {
				received := make(chan map[pp.ExtensionName]pp.ExtensionNumber, 10)
				cfg := TestingConfig(t)
				// One TCP conn is enough, as in TestUserLtep. A uTP packet arriving while
				// Client.Close runs its socket closers under the client lock can deadlock with
				// the uTP firewall callback, which takes the client lock too.
				cfg.DisableUTP = true
				cfg.DisableIPv6 = true
				cfg.Callbacks.ReadExtendedHandshake = func(_ *PeerConn, msg *pp.ExtendedHandshakeMessage) {
					// Called with the client lock held, so it mustn't block. Only the first matters.
					select {
					case received <- msg.M:
					default:
					}
				}
				cl, err := NewClient(cfg)
				qt.Assert(t, qt.IsNil(err))
				t.Cleanup(func() { cl.Close() })
				tt, _ := cl.AddTorrentOpt(opts)
				tt.DownloadAll()
				// A torrent doesn't want conns while its pieces are being checked, and a dial
				// it refuses then isn't retried. Finish the check first.
				qt.Assert(t, qt.IsNil(tt.VerifyData()))
				return cl, received
			}
			cl1, fromCl2 := newClient()
			cl2, fromCl1 := newClient()
			t1, _ := cl1.Torrent(opts.InfoHash)
			qt.Assert(t, qt.Not(qt.Equals(t1.AddClientPeer(cl2), 0)))
			for _, received := range []<-chan map[pp.ExtensionName]pp.ExtensionNumber{fromCl1, fromCl2} {
				select {
				case m := <-received:
					qt.Check(t, qt.Not(qt.Equals(m[pp.ExtensionNameMetadata], 0)))
					_, offersPex := m[pp.ExtensionNamePex]
					qt.Check(t, qt.Equals(offersPex, !private))
				case <-time.After(10 * time.Second):
					t.Fatal("no extended handshake")
				}
			}
		})
	}
}

// Another node's DHT announce_peer for a private torrent's infohash doesn't give it a peer: BEP 27
// allows only the tracker's.
func TestDhtAnnouncePeerIgnoredForPrivateTorrent(t *testing.T) {
	for _, private := range []bool{false, true} {
		name := "public"
		if private {
			name = "private"
		}
		t.Run(name, func(t *testing.T) {
			cl, err := NewClient(TestingConfig(t))
			qt.Assert(t, qt.IsNil(err))
			defer cl.Close()
			infoBytes := greetingInfoBytes(t, private)
			tt, _ := cl.AddTorrentOpt(AddTorrentOpts{
				InfoHash:  metainfo.HashBytes(infoBytes),
				InfoBytes: infoBytes,
			})
			// The torrent doesn't want conns, so a peer that is added isn't dialed.
			cl.onDHTAnnouncePeer(tt.InfoHash(), net.IPv4(192, 0, 2, 1), 6881, true)
			cl.rLock()
			defer cl.rUnlock()
			added := 1
			if private {
				added = 0
			}
			qt.Check(t, qt.Equals(tt.peers.Len(), added))
		})
	}
}
