package httpTracker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	qt "github.com/go-quicktest/qt"
)

// Announces to a tracker served by handler.
func announceTo(t *testing.T, handler http.HandlerFunc) error {
	s := httptest.NewServer(handler)
	defer s.Close()
	u, err := url.Parse(s.URL + "/announce")
	qt.Assert(t, qt.IsNil(err))
	_, err = NewClient(u, NewClientOpts{}).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
	return err
}

// Only a bencoded "failure reason" is a refusal: the tracker was reached and answered no. Callers
// tell it from transport and HTTP errors with errors.Is.
func TestAnnounceFailureReasonIsDistinguishable(t *testing.T) {
	err := announceTo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("d14:failure reason22:torrent not registerede"))
	})
	qt.Assert(t, qt.IsNotNil(err))
	qt.Check(t, qt.IsTrue(errors.Is(err, ErrFailureReason)))
	// The message is what it was before the sentinel, reason included.
	qt.Check(t, qt.Equals(err.Error(), `tracker gave failure reason: "torrent not registered"`))

	for _, c := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"http error", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "d14:failure reason4:busye", http.StatusServiceUnavailable)
		}},
		{"undecodable body", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html>"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := announceTo(t, c.handler)
			qt.Assert(t, qt.IsNotNil(err))
			qt.Check(t, qt.IsFalse(errors.Is(err, ErrFailureReason)))
		})
	}

	t.Run("timeout", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer s.Close()
		u, err := url.Parse(s.URL + "/announce")
		qt.Assert(t, qt.IsNil(err))
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err = NewClient(u, NewClientOpts{}).Announce(ctx, AnnounceRequest{}, AnnounceOpt{})
		qt.Assert(t, qt.IsNotNil(err))
		qt.Check(t, qt.IsTrue(errors.Is(err, context.DeadlineExceeded)))
		qt.Check(t, qt.IsFalse(errors.Is(err, ErrFailureReason)))
	})

	t.Run("connection refused", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		qt.Assert(t, qt.IsNil(err))
		addr := l.Addr().String()
		l.Close()
		u, err := url.Parse("http://" + addr + "/announce")
		qt.Assert(t, qt.IsNil(err))
		_, err = NewClient(u, NewClientOpts{}).Announce(context.Background(), AnnounceRequest{}, AnnounceOpt{})
		qt.Assert(t, qt.IsNotNil(err))
		qt.Check(t, qt.IsFalse(errors.Is(err, ErrFailureReason)))
		qt.Check(t, qt.IsTrue(strings.Contains(err.Error(), "refused")))
	})
}
