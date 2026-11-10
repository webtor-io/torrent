package shared

import (
	"net/url"
	"slices"
	"strings"
)

// What RedactUrl returns for a string url.Parse refuses: no part of it can be told safe to keep.
const unparsableUrl = "<unparsable url>"

// RedactUrl returns a tracker URL reduced to scheme://host[:port], for logs and error strings.
// Private trackers carry the account's key in the userinfo, the path or the query (for UDP
// trackers that is BEP 41 URL data), and whoever can read the logs could announce as that
// account.
func RedactUrl(rawUrl string) string {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return unparsableUrl
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// RedactUrlError returns err with the URL that a *url.Error quotes reduced by RedactUrl. net/http
// returns a *url.Error for every failed request and url.Parse for every refused string, so err
// must come straight from either: a wrapper has already copied the URL into its own text. The
// cause is kept, so errors.Is and errors.As still see context.Canceled or a net.Error.
func RedactUrlError(err error) error {
	ue, ok := err.(*url.Error)
	if !ok {
		return err
	}
	return &url.Error{Op: ue.Op, URL: RedactUrl(ue.URL), Err: ue.Err}
}

const (
	// Bounds the tracker's own text that goes into an error and from there into logs. Refusals
	// ("unregistered torrent") are a few dozen bytes, and operators classify them by that text, so
	// this keeps them whole.
	MaxTrackerTextLen = 256
	// The shortest part of a tracker URL that RedactEcho replaces. Keys are longer, and shorter parts
	// ("1", "uid") are as often ordinary words of the tracker's text.
	minEchoLen = 8
)

// TruncateTrackerText cuts s to MaxTrackerTextLen bytes, and marks the cut with "...".
func TruncateTrackerText(s string) string {
	if len(s) <= MaxTrackerTextLen {
		return s
	}
	return s[:MaxTrackerTextLen] + "..."
}

// RedactEcho returns text a tracker sent with every echo of u replaced by "<redacted>": the whole
// URL, its userinfo, and each userinfo, path or query part of at least minEchoLen bytes, as sent
// or unescaped. Trackers quote a refused key back ("Invalid passkey (<key>)"), and only the client
// knows which parts of its URL to look for. Call it before TruncateTrackerText: a cut through an
// echo would leave a prefix that no longer matches.
func RedactEcho(text string, u *url.URL) string {
	parts := []string{u.String(), u.User.String()}
	split := func(s, seps string, unescape func(string) (string, error)) {
		for _, p := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
			parts = append(parts, p)
			if p, err := unescape(p); err == nil {
				parts = append(parts, p)
			}
		}
	}
	split(u.User.String(), ":", url.PathUnescape)
	split(u.EscapedPath(), "/", url.PathUnescape)
	// In a query, and only there, "+" stands for a space.
	split(u.RawQuery, "&=", url.QueryUnescape)
	// Longest first, so a shorter part inside a longer echo can't leave pieces of it.
	slices.SortFunc(parts, func(a, b string) int { return len(b) - len(a) })
	for _, p := range parts {
		if len(p) >= minEchoLen {
			text = strings.ReplaceAll(text, p, "<redacted>")
		}
	}
	return text
}
