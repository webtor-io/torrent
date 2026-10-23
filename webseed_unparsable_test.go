package torrent

import (
	"net"
	"testing"

	"github.com/go-quicktest/qt"
)

// A webseed URL that does not parse must not become a peer: its nil RemoteAddr
// made banPeerIP panic for the whole client.
func TestAddWebSeedSkipsUnparsableURL(t *testing.T) {
	cl := newTestingClient(t)
	tor := cl.newTorrentForTesting()
	tor.AddWebSeeds([]string{"http://a/%zz", "http://example.com/f"})
	cl.lock()
	defer cl.unlock()
	qt.Check(t, qt.HasLen(tor.webSeeds, 1))
	cl.banPeerIP(net.ParseIP("192.0.2.1"))
}
