package torrent

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anacrolix/torrent/internal/testutil"
)

func TestReaderReadContext(t *testing.T) {
	cl, err := NewClient(TestingConfig(t))
	require.NoError(t, err)
	defer cl.Close()
	tt, err := cl.AddTorrent(testutil.GreetingMetaInfo())
	require.NoError(t, err)
	defer tt.Drop()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Millisecond))
	defer cancel()
	r := tt.Files()[0].NewReader()
	defer r.Close()
	_, err = r.ReadContext(ctx, make([]byte, 1))
	require.EqualValues(t, context.DeadlineExceeded, err)
}

func TestReaderSetContextAndRead(t *testing.T) {
	cl, err := NewClient(TestingConfig(t))
	require.NoError(t, err)
	defer cl.Close()
	tt, err := cl.AddTorrent(testutil.GreetingMetaInfo())
	require.NoError(t, err)
	defer tt.Drop()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Millisecond))
	defer cancel()
	r := tt.Files()[0].NewReader()
	defer r.Close()
	r.SetContext(ctx)
	_, err = r.Read(make([]byte, 1))
	require.EqualValues(t, context.DeadlineExceeded, err)
}

type recordingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Message)
	h.mu.Unlock()
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// A read abandoned by its caller returns the context error and does not go
// through the storage-failure recovery (reset, completion resync, retry), which
// logged three errors per abandoned read.
func TestReaderCancelledReadIsNotAStorageFailure(t *testing.T) {
	cfg := TestingConfig(t)
	h := &recordingHandler{}
	cfg.Slogger = slog.New(h)
	cl, err := NewClient(cfg)
	require.NoError(t, err)
	defer cl.Close()
	tt, err := cl.AddTorrent(testutil.GreetingMetaInfo())
	require.NoError(t, err)
	defer tt.Drop()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Millisecond))
	defer cancel()
	r := tt.Files()[0].NewReader()
	defer r.Close()
	r.SetContext(ctx)
	_, err = r.Read(make([]byte, 1))
	require.EqualValues(t, context.DeadlineExceeded, err)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		switch m {
		case "initial read failed", "read failed after reader reset", "read failed after completion resync":
			t.Fatalf("cancelled read went through storage-failure recovery: %q", m)
		}
	}
}
