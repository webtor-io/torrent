package httpTracker

import (
	"context"
	"expvar"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/anacrolix/missinggo/httptoo"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/tracker/shared"
	"github.com/anacrolix/torrent/tracker/udp"
	"github.com/anacrolix/torrent/version"
)

var vars = expvar.NewMap("tracker/http")

// We send no numwant, so a tracker returns its default number of peers, commonly 50. A compact
// reply spends 6 bytes on an IPv4 peer and 18 on an IPv6 one, a dictionary reply about 70, so even
// thousands of peers fit far below this. It bounds what a tracker can make us buffer.
const maxResponseBodyLen = 1 << 20

// The tracker's own text, fit for an error: it can quote our key back, and is as long as the
// tracker likes.
func (cl Client) trackerText(s string) string {
	return shared.TruncateTrackerText(shared.RedactEcho(s, cl.url_))
}

func setAnnounceParams(_url *url.URL, ar *AnnounceRequest, opts AnnounceOpt) {
	q := url.Values{}

	q.Set("key", strconv.FormatInt(int64(ar.Key), 10))
	q.Set("info_hash", string(ar.InfoHash[:]))
	q.Set("peer_id", string(ar.PeerId[:]))
	// AFAICT, port is mandatory, and there's no implied port key.
	q.Set("port", fmt.Sprintf("%d", ar.Port))
	q.Set("uploaded", strconv.FormatInt(ar.Uploaded, 10))
	q.Set("downloaded", strconv.FormatInt(ar.Downloaded, 10))

	// The AWS S3 tracker returns "400 Bad Request: left(-1) was not in the valid range 0 -
	// 9223372036854775807" if left is out of range, or "500 Internal Server Error: Internal Server
	// Error" if omitted entirely.
	left := ar.Left
	if left < 0 {
		left = math.MaxInt64
	}
	q.Set("left", strconv.FormatInt(left, 10))

	if ar.Event != shared.None {
		q.Set("event", ar.Event.String())
	}
	// http://stackoverflow.com/questions/17418004/why-does-tracker-server-not-understand-my-request-bittorrent-protocol
	q.Set("compact", "1")
	// According to https://wiki.vuze.com/w/Message_Stream_Encryption. TODO:
	// Take EncryptionPolicy or something like it as a parameter.
	q.Set("supportcrypto", "1")
	doIp := func(versionKey string, ip net.IP) {
		if ip == nil {
			return
		}
		ipString := ip.String()
		q.Set(versionKey, ipString)
		// Let's try listing them. BEP 3 mentions having an "ip" param, and BEP 7 says we can list
		// addresses for other address-families, although it's not encouraged.
		q.Add("ip", ipString)
	}
	doIp("ipv4", opts.ClientIp4)
	doIp("ipv6", opts.ClientIp6)
	// We're operating purely on query-escaped strings, where + would have already been encoded to
	// %2B, and + has no other special meaning. See https://github.com/anacrolix/torrent/issues/534.
	qstr := strings.ReplaceAll(q.Encode(), "+", "%20")

	// Some private trackers require the original query param to be in the first position.
	if _url.RawQuery != "" {
		_url.RawQuery += "&" + qstr
	} else {
		_url.RawQuery = qstr
	}
}

type AnnounceOpt struct {
	UserAgent           string
	HostHeader          string
	ClientIp4           net.IP
	ClientIp6           net.IP
	HttpRequestDirector func(*http.Request) error
}

type AnnounceRequest = udp.AnnounceRequest

func (cl Client) Announce(ctx context.Context, ar AnnounceRequest, opt AnnounceOpt) (ret AnnounceResponse, err error) {
	_url := httptoo.CopyURL(cl.url_)
	setAnnounceParams(_url, &ar, opt)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, _url.String(), nil)
	userAgent := opt.UserAgent
	if userAgent == "" {
		userAgent = version.DefaultHttpUserAgent
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	if opt.HttpRequestDirector != nil {
		err = opt.HttpRequestDirector(req)
		if err != nil {
			err = fmt.Errorf("error modifying HTTP request: %w", err)
			return
		}
	}

	req.Host = opt.HostHeader
	resp, err := cl.hc.Do(req)
	if err != nil {
		// The error quotes the request URL: a private tracker's key, and our announce parameters.
		err = shared.RedactUrlError(err)
		return
	}
	defer resp.Body.Close()
	// As before, a read error is not checked: a cut body fails to decode below.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyLen+1))
	// Neither the body nor the tracker's reason phrase goes into errors: either can echo the
	// request URL, and both are as long as the tracker likes.
	status := fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	if len(body) > maxResponseBodyLen {
		err = fmt.Errorf("response from tracker: %s: body exceeds %d bytes", status, maxResponseBodyLen)
		return
	}
	if resp.StatusCode != 200 {
		err = fmt.Errorf("response from tracker: %s: %d bytes", status, len(body))
		return
	}
	var trackerResponse HttpResponse
	err = bencode.Unmarshal(body, &trackerResponse)
	if _, ok := err.(bencode.ErrUnusedTrailingBytes); ok {
		err = nil
	} else if err != nil {
		// The decoder quotes dict keys, which are the tracker's.
		err = fmt.Errorf("error decoding response from tracker (%d bytes): %s", len(body), cl.trackerText(err.Error()))
		return
	}
	if trackerResponse.FailureReason != "" {
		err = fmt.Errorf("tracker gave failure reason: %q", cl.trackerText(trackerResponse.FailureReason))
		return
	}
	vars.Add("successful http announces", 1)
	ret.Interval = trackerResponse.Interval
	ret.Leechers = trackerResponse.Incomplete
	ret.Seeders = trackerResponse.Complete
	if len(trackerResponse.Peers.List) != 0 {
		vars.Add("http responses with nonempty peers key", 1)
	}
	ret.Peers = trackerResponse.Peers.List
	if len(trackerResponse.Peers6) != 0 {
		vars.Add("http responses with nonempty peers6 key", 1)
	}
	for _, na := range trackerResponse.Peers6 {
		ret.Peers = append(ret.Peers, Peer{
			IP:   na.IP,
			Port: na.Port,
		})
	}
	return
}

type AnnounceResponse struct {
	Interval int32 // Minimum seconds the local peer should wait before next announce.
	Leechers int32
	Seeders  int32
	Peers    []Peer
}
