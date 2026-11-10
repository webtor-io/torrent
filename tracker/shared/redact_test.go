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

func TestRedactEcho(t *testing.T) {
	// How trackers commonly refuse a key, quoting it back.
	refusal := "Invalid passkey (33 - " + testKey + ")"
	for _, c := range []struct {
		name, url, text, want string
	}{
		{"path", "http://tracker.invalid/" + testKey + "/announce", refusal, "Invalid passkey (33 - <redacted>)"},
		{"query", "http://tracker.invalid/a?passkey=" + testKey, refusal, "Invalid passkey (33 - <redacted>)"},
		{"userinfo", "http://u:" + testKey + "@tracker.invalid/a", refusal, "Invalid passkey (33 - <redacted>)"},
		{"whole userinfo", "http://u:" + testKey + "@tracker.invalid/a", "bad login u:" + testKey, "bad login <redacted>"},
		{"query without value", "udp://192.0.2.1:6969/a?" + testKey, refusal, "Invalid passkey (33 - <redacted>)"},
		// The tracker may quote a part as sent or as it decoded it.
		{"path as sent", "http://tracker.invalid/01234567%2B89/a", "bad 01234567%2B89", "bad <redacted>"},
		{"path unescaped", "http://tracker.invalid/01234567%2B89/a", "bad 01234567+89", "bad <redacted>"},
		{"query as sent", "http://tracker.invalid/a?pk=01234567+89", "bad 01234567+89", "bad <redacted>"},
		{"query unescaped", "http://tracker.invalid/a?pk=01234567+89", "bad 01234567 89", "bad <redacted>"},
		{"userinfo unescaped", "http://u:01234567%4089@tracker.invalid/a", "bad 01234567@89", "bad <redacted>"},
		{"whole url", "http://tracker.invalid/a?b=1", "GET http://tracker.invalid/a?b=1 refused", "GET <redacted> refused"},
		// The path segment is the key's first bytes: replacing it first would leave the rest.
		{"longest first", "http://tracker.invalid/01234567/a?pk=" + testKey, "bad " + testKey, "bad <redacted>"},
		{"every occurrence", "http://tracker.invalid/a?pk=" + testKey, testKey + " " + testKey, "<redacted> <redacted>"},
		// Short parts are ordinary words of the tracker's text as often as not.
		{"short values", "http://tracker.invalid/t/a.php?id=1&uid=42", "id 1 for uid 42 at t/a.php unknown", "id 1 for uid 42 at t/a.php unknown"},
		// Every tracker URL ends in one of these, and refusals name them.
		{"announce kept", "http://tracker.invalid/" + testKey + "/announce", "announce refused: " + refusal, "announce refused: Invalid passkey (33 - <redacted>)"},
		{"common segments in any case", "http://tracker.invalid/" + testKey + "/SCRAPE.php/Announce.PHP", "Announce.PHP, SCRAPE.php: " + testKey, "Announce.PHP, SCRAPE.php: <redacted>"},
		{"key starting with announce", "http://tracker.invalid/announce0123/a", "bad announce0123", "bad <redacted>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.url)
			if err != nil {
				t.Fatal(err)
			}
			if got := RedactEcho(c.text, u); got != c.want {
				t.Errorf("RedactEcho(%q, %q) = %q, want %q", c.text, c.url, got, c.want)
			}
		})
	}
}

func TestTruncateTrackerText(t *testing.T) {
	short := strings.Repeat("x", MaxTrackerTextLen)
	if got := TruncateTrackerText(short); got != short {
		t.Errorf("cut text of %d bytes: %q", len(short), got)
	}
	if got := TruncateTrackerText(short + "y"); got != short+"..." {
		t.Errorf("got %q", got)
	}
}
