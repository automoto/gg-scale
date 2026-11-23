package webutil

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
)

// CrossOriginProtection refuses a state-changing browser request from
// another origin, in addition to the CSRF value. A request with no Origin
// and no Sec-Fetch-Site header passes (non-browser clients). The origin of
// baseURL, the public URL of the site, is trusted too, so the site's own
// forms still work behind a proxy that changes the Host header. A baseURL
// that is not an absolute http(s) URL is logged and not trusted.
func CrossOriginProtection(baseURL string) func(http.Handler) http.Handler {
	p := http.NewCrossOriginProtection()
	if baseURL != "" {
		if err := trustBaseOrigin(p, baseURL); err != nil {
			slog.Warn("cross-origin check: CONTROL_PANEL_BASE_URL is not trusted", "err", err)
		}
	}
	return p.Handler
}

func trustBaseOrigin(p *http.CrossOriginProtection, baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("not an absolute http(s) URL")
	}
	return p.AddTrustedOrigin(u.Scheme + "://" + u.Host)
}
