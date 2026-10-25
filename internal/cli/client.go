package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// client talks to one install's API.
type client struct {
	host  string
	token string
	agent string
	http  *http.Client
}

// apiErr is an error the API answered with, in its own words.
type apiErr struct {
	Status  int
	Code    string
	Message string
}

func (e *apiErr) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the server answered %d", e.Status)
}

// do sends one request and decodes the JSON answer into out, when out is
// not nil. A non-2xx answer is an *apiErr carrying the server's message.
func (c *client) do(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	resp, err := c.send(ctx, method, path, body, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return readAPIErr(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("the server's answer could not be read: %w", err)
	}
	return nil
}

func (c *client) send(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.host, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.agent)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("could not reach %s: %v", c.host, ue.Err)
		}
		return nil, err
	}
	return resp, nil
}

func readAPIErr(resp *http.Response) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return &apiErr{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 200 || strings.HasPrefix(msg, "<") {
		msg = ""
	}
	return &apiErr{Status: resp.StatusCode, Message: msg}
}

func (c *client) getJSON(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, "", out)
}

func (c *client) postJSON(ctx context.Context, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	return c.do(ctx, http.MethodPost, path, body, "application/json", out)
}

// The API's shapes, as much of them as the CLI reads.
type (
	meView struct {
		Team struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"team"`
		Role  string `json:"role"`
		Token *struct {
			Name      string `json:"name"`
			CreatedBy string `json:"created_by"`
		} `json:"token"`
		Project *struct {
			Slug string `json:"slug"`
		} `json:"project"`
	}
	projectView struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	appView struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Source   string `json:"source"`
		Image    string `json:"image"`
		Replicas int32  `json:"replicas"`
		URL      string `json:"url"`
		Status   struct {
			Known   bool   `json:"known"`
			Phase   string `json:"phase"`
			Ready   int32  `json:"ready"`
			Desired int32  `json:"desired"`
			Message string `json:"message"`
		} `json:"status"`
		Dashboard string          `json:"dashboard_url"`
		Latest    *deploymentView `json:"latest_deployment"`
	}
	deploymentView struct {
		ID         string     `json:"id"`
		Title      string     `json:"title"`
		Badge      string     `json:"badge"`
		Status     string     `json:"status"`
		Source     string     `json:"source"`
		Actor      string     `json:"actor"`
		Message    string     `json:"message"`
		StartedAt  time.Time  `json:"started_at"`
		FinishedAt *time.Time `json:"finished_at"`
		Upload     *struct {
			Bytes int64 `json:"bytes"`
		} `json:"upload"`
	}
	variableView struct {
		Key    string `json:"key"`
		Value  string `json:"value"`
		Secret bool   `json:"secret"`
		Masked bool   `json:"masked"`
		Staged string `json:"staged"`
	}
)
