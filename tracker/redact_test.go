package tracker

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anacrolix/torrent/tracker/shared"
	"github.com/anacrolix/torrent/tracker/udp"
)

// A fake private-tracker key. For a UDP tracker it travels as BEP 41 URL data.
const testKey = "0123456789abcdef0123456789abcdef"

// Collects every message of a log.Logger, Debug included.
type logCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *logCapture) Handle(r log.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.WriteString(r.Msg.String())
	c.buf.WriteByte('\n')
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *logCapture) logger() log.Logger {
	l := log.NewLogger()
	l.SetHandlers(c)
	return l.WithFilterLevel(log.Debug)
}

func TestUdpAnnounceLogOmitsUrlData(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer pc.Close()
	go func() {
		var b [512]byte
		n, addr, err := pc.ReadFrom(b[:])
		if err != nil {
			return
		}
		var h udp.RequestHeader
		udp.Read(bytes.NewReader(b[:n]), &h)
		// Answer a transaction the client never started: it logs the packet and keeps waiting.
		var w bytes.Buffer
		udp.Write(&w, udp.ResponseHeader{Action: udp.ActionConnect, TransactionId: h.TransactionId + 1})
		udp.Write(&w, udp.ConnectionResponse{ConnectionId: 42})
		pc.WriteTo(w.Bytes(), addr)
	}()
	var logs logCapture
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = Announce{
		TrackerUrl: "udp://" + pc.LocalAddr().String() + "/" + testKey + "/announce?passkey=" + testKey,
		Context:    ctx,
		Logger:     logs.logger(),
	}.Do()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testKey)
	out := logs.String()
	// The line that carries the client's context.
	require.Contains(t, out, "dispatching packet")
	assert.Contains(t, out, "udp://"+pc.LocalAddr().String())
	assert.NotContains(t, out, testKey)
	assert.NotContains(t, out, "/announce")
}

func TestNewClientParseErrorOmitsUrl(t *testing.T) {
	_, err := NewClient("http://tracker.invalid/"+testKey+"/announce?passkey="+testKey+"\x7f", NewClientOpts{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testKey)
	assert.NotContains(t, err.Error(), "/announce")
}

// A UDP tracker that hands out a connection ID and answers every announce with an error carrying
// msg.
func udpErrorTracker(t *testing.T, msg string) string {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { pc.Close() })
	go func() {
		var b [2048]byte
		for {
			n, addr, err := pc.ReadFrom(b[:])
			if err != nil {
				return
			}
			var h udp.RequestHeader
			if udp.Read(bytes.NewReader(b[:n]), &h) != nil {
				continue
			}
			var w bytes.Buffer
			if h.Action == udp.ActionConnect {
				udp.Write(&w, udp.ResponseHeader{Action: udp.ActionConnect, TransactionId: h.TransactionId})
				udp.Write(&w, udp.ConnectionResponse{ConnectionId: 42})
			} else {
				udp.Write(&w, udp.ResponseHeader{Action: udp.ActionError, TransactionId: h.TransactionId})
				w.WriteString(msg)
			}
			pc.WriteTo(w.Bytes(), addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestUdpErrorResponseRedactsEchoedKey(t *testing.T) {
	// The tracker got the path and query as URL data, and quotes the key back past where the
	// message is cut.
	msg := "Invalid passkey (33 - " + testKey + ")" + strings.Repeat("x", 2000)
	addr := udpErrorTracker(t, msg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Announce{
		TrackerUrl: "udp://" + addr + "/" + testKey + "/announce?passkey=" + testKey,
		Context:    ctx,
	}.Do()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testKey)
	assert.True(t, strings.HasPrefix(err.Error(), "error response: `Invalid passkey (33 - <redacted>)xxx"), "%v", err)
	assert.LessOrEqual(t, len(err.Error()), len("error response: ``...")+shared.MaxTrackerTextLen)
}
