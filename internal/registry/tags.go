package registry

// Tag listing: every tag a repository has, which is how Yacht learns that
// postgres:16.16-alpine exists before anybody types it.
//
// Asked with the same client, the same two-attempt challenge and the same
// redirect and realm rules as a manifest, so listing a repository cannot send
// the install's credential anywhere resolving one of its images would not.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// tagsLimit caps one page of a tag list. Docker Hub answers library/mongo,
// the longest of the official databases, with about 90 KB in one page.
const tagsLimit = 8 << 20

// tagPages bounds how many pages are followed: a registry that keeps naming
// a next page is not allowed to keep Yacht reading.
const tagPages = 10

// Tags lists a repository's tags. Only the repository part of the reference
// is read: "postgres:16-alpine" lists library/postgres on Docker Hub.
func (s *Store) Tags(ctx context.Context, ref string) ([]string, error) {
	parsed, err := parseReference(ref)
	if err != nil {
		return nil, err
	}
	client, err := s.clientForManifest(ctx, parsed)
	if err != nil {
		return nil, err
	}
	return client.tags(ctx, parsed)
}

// tags reads /v2/<name>/tags/list, following the Link header the
// distribution spec pages with.
func (m manifestClient) tags(ctx context.Context, ref reference) ([]string, error) {
	base := m.registryURL(ref)
	next := base.JoinPath("v2", ref.name, "tags", "list")
	var out []string
	var authorization string
	for page := 0; next != nil; page++ {
		if page == tagPages {
			return nil, fmt.Errorf("%w: %s named more than %d pages of tags",
				ErrUnreachable, ref.host, tagPages)
		}
		var names []string
		var err error
		names, next, authorization, err = m.tagPage(ctx, ref, next, authorization)
		if err != nil {
			return nil, err
		}
		out = append(out, names...)
	}
	return out, nil
}

// tagPage reads one page, answering a challenge once. The authorization it
// returns is reused for the pages after, as a token is scoped to the
// repository rather than to one request.
func (m manifestClient) tagPage(
	ctx context.Context, ref reference, endpoint *url.URL, authorization string,
) ([]string, *url.URL, string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, nil, "", fmt.Errorf("registry: build tags request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := m.do(req)
		if err != nil {
			return nil, nil, "", fmt.Errorf("%w: %s: %w", ErrUnreachable, ref.host, err)
		}
		if resp.StatusCode == http.StatusOK {
			var body struct {
				Tags []string `json:"tags"`
			}
			err := json.NewDecoder(io.LimitReader(resp.Body, tagsLimit)).Decode(&body)
			link := resp.Header.Get("Link")
			resp.Body.Close() //nolint:errcheck
			if err != nil {
				return nil, nil, "", fmt.Errorf("%w: %s answered a tag list that is not one: %w",
					ErrUnreachable, ref.host, err)
			}
			next, err := nextPage(endpoint, link)
			if err != nil {
				return nil, nil, "", fmt.Errorf("%w: %s: %w", ErrUnreachable, ref.host, err)
			}
			return body.Tags, next, authorization, nil
		}
		challenge := resp.Header.Get("WWW-Authenticate")
		status, statusText := resp.StatusCode, resp.Status
		io.Copy(io.Discard, io.LimitReader(resp.Body, 8192)) //nolint:errcheck
		resp.Body.Close()                                    //nolint:errcheck
		if status == http.StatusUnauthorized && attempt == 0 {
			next, err := m.authorize(ctx, challenge, ref)
			if err != nil {
				return nil, nil, "", err
			}
			if next == "" || next == authorization {
				return nil, nil, "", fmt.Errorf("%w: %s", ErrCredentialsRejected, ref.host)
			}
			authorization = next
			continue
		}
		return nil, nil, "", classifyStatus(status, statusText, ref)
	}
	return nil, nil, "", fmt.Errorf("%w: %s", ErrCredentialsRejected, ref.host)
}

// nextPage is the page a Link header names, `</v2/x/tags/list?last=b>;
// rel="next"`, or nil when there is none. Only on the registry the list came
// from: a next page elsewhere would be a way to aim the credential at it.
func nextPage(current *url.URL, link string) (*url.URL, error) {
	if link == "" {
		return nil, nil
	}
	for _, part := range strings.Split(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(strings.ReplaceAll(params, " ", ""), `rel="next"`) {
			continue
		}
		target = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(target), "<"), ">")
		u, err := current.Parse(target)
		if err != nil {
			return nil, fmt.Errorf("a next page of %q: %w", target, err)
		}
		if !sameAuthority(u, current) || u.Scheme != current.Scheme {
			return nil, fmt.Errorf("refused a next page on %s", authorityOf(u))
		}
		return u, nil
	}
	return nil, nil
}
