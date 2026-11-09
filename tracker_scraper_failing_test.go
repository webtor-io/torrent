package torrent

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestAllAnnouncesFailing(t *testing.T) {
	failed := func() torrentTrackerAnnouncer {
		return &trackerScraper{
			stopCh:       make(chan struct{}),
			lastAnnounce: trackerAnnounceResult{Err: errors.New("refused"), Completed: time.Now()},
		}
	}
	answered := func() torrentTrackerAnnouncer {
		return &trackerScraper{
			stopCh:       make(chan struct{}),
			lastAnnounce: trackerAnnounceResult{NumPeers: 2, Completed: time.Now()},
		}
	}
	pending := func() torrentTrackerAnnouncer {
		return &trackerScraper{stopCh: make(chan struct{})}
	}
	websocket := func() torrentTrackerAnnouncer {
		return websocketTrackerStatus{}
	}

	cases := []struct {
		name       string
		announcers []func() torrentTrackerAnnouncer
		want       bool
	}{
		{"no trackers", nil, false},
		{"the only tracker failed", []func() torrentTrackerAnnouncer{failed}, true},
		{"every tracker failed", []func() torrentTrackerAnnouncer{failed, failed}, true},
		{"one tracker still answers", []func() torrentTrackerAnnouncer{failed, answered}, false},
		{"one tracker has not answered yet", []func() torrentTrackerAnnouncer{failed, pending}, false},
		{"a websocket tracker's outcome is unknown", []func() torrentTrackerAnnouncer{failed, websocket}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := newTestingClient(t)
			tt, _ := cl.AddTorrentInfoHash(testingTorrentInfoHash)
			cl.lock()
			for i, newAnnouncer := range c.announcers {
				if tt.trackerAnnouncers == nil {
					tt.trackerAnnouncers = map[torrentTrackerAnnouncerKey]torrentTrackerAnnouncer{}
				}
				tt.trackerAnnouncers[torrentTrackerAnnouncerKey{url: fmt.Sprint(i)}] = newAnnouncer()
			}
			cl.unlock()
			if got := tt.AllAnnouncesFailing(); got != c.want {
				t.Errorf("AllAnnouncesFailing() = %v, want %v", got, c.want)
			}
		})
	}
}
