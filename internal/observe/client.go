package observe

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// storeURL parses a store's address. Empty is no store; anything else must be
// an http or https URL, refused at startup rather than at the first page that
// reads from it.
func storeURL(name, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s must be an http or https URL, such as http://victoria-logs:9428", name)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s must not carry a query or fragment", name)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// defaultClient bounds connecting, not reading: a live tail is one response
// that does not end, so the whole-request timeout is left to each caller's
// context.
func defaultClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
	}}
}

// post sends a form to a store and returns the body of a successful answer.
// A failed one is read for the store's own reason, which for a malformed
// query says exactly what was malformed.
func post(ctx context.Context, client *http.Client, base *url.URL, path string, form url.Values) (io.ReadCloser, error) {
	u := *base
	u.Path += path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("observe: %s: %w", base.Host, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("observe: %s answered %d: %s", base.Host, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp.Body, nil
}

// promDuration writes a duration the way LogsQL and PromQL both read one:
// whole seconds, minutes, hours or days.
func promDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	s := int64(d / time.Second)
	if s < 1 {
		s = 1
	}
	return strconv.FormatInt(s, 10) + "s"
}
