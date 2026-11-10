package shared

import (
	"net/url"
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
