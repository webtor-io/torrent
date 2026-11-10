package shared

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

// A fake private-tracker key. Nothing derived from it may survive redaction.
const testKey = "0123456789abcdef0123456789abcdef"

func TestRedactUrl(t *testing.T) {
	for _, c := range []struct {
		in, want string
	}{
		{"https://tracker.invalid/announce?passkey=" + testKey, "https://tracker.invalid"},
		{"http://tracker.invalid:2710/" + testKey + "/announce", "http://tracker.invalid:2710"},
		{"http://user:" + testKey + "@tracker.invalid/announce", "http://tracker.invalid"},
		{"http://tracker.invalid/announce?uk=x&pk=" + testKey + "#" + testKey, "http://tracker.invalid"},
		// BEP 41 puts the path and query of a UDP tracker URL on the wire as URL data.
		{"udp4://192.0.2.1:6969/announce?" + testKey, "udp4://192.0.2.1:6969"},
		{"wss://tracker.invalid/" + testKey, "wss://tracker.invalid"},
		{"http://[2001:db8::1]:8080/announce?passkey=" + testKey, "http://[2001:db8::1]:8080"},
		{"https://tracker.invalid", "https://tracker.invalid"},
		// url.Parse refuses control characters, so nothing of this URL can be kept.
		{"http://tracker.invalid/announce?passkey=" + testKey + "\x7f", unparsableUrl},
	} {
		got := RedactUrl(c.in)
		if got != c.want {
			t.Errorf("RedactUrl(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactUrlError(t *testing.T) {
	// What net/http returns when the announce context is cancelled.
	err := RedactUrlError(&url.Error{
		Op:  "Get",
		URL: "http://192.0.2.1:2710/" + testKey + "/announce?passkey=" + testKey + "&info_hash=x",
		Err: context.Canceled,
	})
	if s := err.Error(); strings.Contains(s, testKey) || strings.Contains(s, "announce") {
		t.Errorf("error still quotes the tracker URL: %v", s)
	}
	if want := `Get "http://192.0.2.1:2710": context canceled`; err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
	// Callers decide how to log by the cause, and net/http's errors are net.Errors.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(%v, context.Canceled) = false", err)
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		t.Errorf("errors.As(%v, *url.Error) = false", err)
	}

	// The error url.Parse returns quotes the raw string too.
	_, parseErr := url.Parse("http://tracker.invalid/announce?passkey=" + testKey + "\x7f")
	if parseErr == nil {
		t.Fatal("url.Parse accepted a control character")
	}
	if s := RedactUrlError(parseErr).Error(); strings.Contains(s, testKey) {
		t.Errorf("parse error still quotes the tracker URL: %v", s)
	}

	other := errors.New("tracker gave failure reason")
	if got := RedactUrlError(other); got != other {
		t.Errorf("RedactUrlError changed an error without a URL: %v", got)
	}
	if got := RedactUrlError(nil); got != nil {
		t.Errorf("RedactUrlError(nil) = %v", got)
	}
}
