package torrent

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-quicktest/qt"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	httpTracker "github.com/anacrolix/torrent/tracker/http"
)

var (
	trackerRefusal = string(bencode.MustMarshal(map[string]any{
		"failure reason": "torrent not registered",
	}))
	trackerSuccess = string(bencode.MustMarshal(map[string]any{
		"interval": 1800,
		"peers":    "",
	}))
)

// Stands in for an HTTP 5xx in a scriptedTracker's responses.
const trackerUnavailable = "503"

// An HTTP tracker that answers each announce with the next of its responses, repeating the last.
type scriptedTracker struct {
	responses []string

	mu     sync.Mutex
	events []string
}

func (me *scriptedTracker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	me.mu.Lock()
	resp := me.responses[min(len(me.events), len(me.responses)-1)]
	me.events = append(me.events, r.URL.Query().Get("event"))
	me.mu.Unlock()
	if resp == trackerUnavailable {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte(resp))
}

// The event of every announce received so far.
func (me *scriptedTracker) announceEvents() []string {
	me.mu.Lock()
	defer me.mu.Unlock()
	return append([]string(nil), me.events...)
}

// A wait the trackerScraper asked for, which the test ends by sending on fire.
type scraperWait struct {
	d    time.Duration
	fire chan time.Time
}

type scraperUnderTest struct {
	*trackerScraper
	waits <-chan scraperWait
	// Closed when Run returns.
	done <-chan struct{}
}

// Starts a trackerScraper for tt announcing to trackerUrl, like Torrent.startScrapingTracker does,
// but on a clock the test drives through waits.
func startTestScraper(t *testing.T, tt *Torrent, trackerUrl string) scraperUnderTest {
	u, err := url.Parse(trackerUrl)
	qt.Assert(t, qt.IsNil(err))
	waits := make(chan scraperWait, 100)
	ts := &trackerScraper{
		shortInfohash: *tt.canonicalShortInfohash(),
		u:             *u,
		t:             tt,
		stopCh:        make(chan struct{}),
		logger:        tt.slogger(),
		after: func(d time.Duration) <-chan time.Time {
			fire := make(chan time.Time, 1)
			waits <- scraperWait{d, fire}
			return fire
		},
	}
	tt.cl.lock()
	tt.trackerAnnouncers = map[torrentTrackerAnnouncerKey]torrentTrackerAnnouncer{
		{shortInfohash: ts.shortInfohash, url: trackerUrl}: ts,
	}
	tt.cl.unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ts.Run()
	}()
	t.Cleanup(func() {
		ts.Stop()
		<-done
	})
	return scraperUnderTest{ts, waits, done}
}

// Lets the scraper run through n waits, and returns how long each was.
func (me scraperUnderTest) runWaits(t *testing.T, n int) (ds []time.Duration) {
	t.Helper()
	for range n {
		select {
		case w := <-me.waits:
			ds = append(ds, w.d)
			w.fire <- time.Now()
		case <-me.done:
			t.Fatalf("scraper stopped after %v waits %v", len(ds), ds)
		case <-time.After(10 * time.Second):
			t.Fatalf("no wait after %v", ds)
		}
	}
	return
}

// Checks the scraper ends with no further wait.
func (me scraperUnderTest) checkStops(t *testing.T) {
	t.Helper()
	select {
	case w := <-me.waits:
		t.Errorf("scraper waits %v for another announce", w.d)
	case <-me.done:
	case <-time.After(10 * time.Second):
		t.Fatal("scraper neither stopped nor waited")
	}
}

// Rounds the waits up to whole minutes. Each is the interval less the time the announce took.
func waitMinutes(ds []time.Duration) (ms []time.Duration) {
	for _, d := range ds {
		ms = append(ms, d.Round(time.Minute))
	}
	return
}

func repeatDuration(d time.Duration, n int) (ds []time.Duration) {
	for range n {
		ds = append(ds, d)
	}
	return
}

func newScraperTestTorrent(t *testing.T, private bool) *Torrent {
	cl := newTestingClient(t)
	infoBytes := greetingInfoBytes(t, private)
	tt, _ := cl.AddTorrentOpt(AddTorrentOpts{
		InfoHash:  metainfo.HashBytes(infoBytes),
		InfoBytes: infoBytes,
	})
	return tt
}

func startScriptedTracker(t *testing.T, responses ...string) (*scriptedTracker, string) {
	st := &scriptedTracker{responses: responses}
	s := httptest.NewServer(st)
	t.Cleanup(s.Close)
	return st, s.URL + "/announce"
}

// A private torrent's tracker that keeps refusing is asked again after 1, 2 and 4 minutes, and then
// not at all, not even to say we stopped. Its last refusal stays on record.
func TestPrivateTrackerRefusalsBackOffThenStop(t *testing.T) {
	tt := newScraperTestTorrent(t, true)
	st, trackerUrl := startScriptedTracker(t, trackerRefusal)
	ts := startTestScraper(t, tt, trackerUrl)
	waits := ts.runWaits(t, maxPrivateTrackerRefusals-1)
	qt.Check(t, qt.DeepEquals(waitMinutes(waits), []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}))
	ts.checkStops(t)
	qt.Check(t, qt.DeepEquals(st.announceEvents(), []string{"started", "", "", ""}))
	qt.Check(t, qt.IsTrue(tt.AllAnnouncesFailing()))
	tt.cl.rLock()
	lastErr := ts.lastAnnounce.Err
	tt.cl.rUnlock()
	qt.Check(t, qt.IsTrue(errors.Is(lastErr, httpTracker.ErrFailureReason)))
}

// A successful announce starts the count of refusals again. Other errors neither count nor reset
// it, and are retried at the floor.
func TestPrivateTrackerRefusalCount(t *testing.T) {
	tt := newScraperTestTorrent(t, true)
	st, trackerUrl := startScriptedTracker(t,
		trackerRefusal, trackerRefusal, trackerSuccess,
		trackerRefusal, trackerRefusal, trackerUnavailable, trackerRefusal, trackerRefusal)
	ts := startTestScraper(t, tt, trackerUrl)
	waits := ts.runWaits(t, 7)
	qt.Check(t, qt.DeepEquals(waitMinutes(waits), []time.Duration{
		time.Minute, 2 * time.Minute,
		// The tracker's own interval after the success.
		30 * time.Minute,
		time.Minute, 2 * time.Minute,
		// The HTTP error.
		time.Minute,
		4 * time.Minute,
	}))
	ts.checkStops(t)
	qt.Check(t, qt.HasLen(st.announceEvents(), 8))
}

// Everything that isn't a private torrent's tracker refusing is retried at the one-minute floor for
// as long as the torrent is loaded, as before.
func TestTrackerFailuresKeepTheFloor(t *testing.T) {
	refusedPort, err := net.Listen("tcp", "127.0.0.1:0")
	qt.Assert(t, qt.IsNil(err))
	unreachableUrl := "http://" + refusedPort.Addr().String() + "/announce"
	refusedPort.Close()

	for _, c := range []struct {
		name     string
		private  bool
		response string
	}{
		{"public torrent refused", false, trackerRefusal},
		{"private torrent, tracker unreachable", true, ""},
		{"private torrent, tracker http error", true, trackerUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			tt := newScraperTestTorrent(t, c.private)
			trackerUrl := unreachableUrl
			if c.response != "" {
				_, trackerUrl = startScriptedTracker(t, c.response)
			}
			ts := startTestScraper(t, tt, trackerUrl)
			n := 2 * maxPrivateTrackerRefusals
			qt.Check(t, qt.DeepEquals(waitMinutes(ts.runWaits(t, n)), repeatDuration(time.Minute, n)))
			qt.Check(t, qt.IsTrue(tt.AllAnnouncesFailing()))
		})
	}
}
