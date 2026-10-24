package notify

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

const cloudflareAPI = "https://api.cloudflare.com/client/v4"

// NewCloudflare returns a Mailer that sends through Cloudflare Email
// Sending's HTTP API, for an account whose domain is onboarded to it.
//
// Cloudflare takes SMTP only on port 465, which many hosts block on the way
// out, as they block 25; its API is HTTPS, which nothing blocks. The token
// needs Email Sending: Edit on the account.
func NewCloudflare(accountID, token, from string) (Mailer, error) {
	var errs []error
	if accountID == "" {
		errs = append(errs, errors.New("notify: Cloudflare account ID is required"))
	} else if strings.ContainsAny(accountID, "/?#") {
		errs = append(errs, fmt.Errorf("notify: Cloudflare account ID %q is not an ID", accountID))
	}
	if token == "" {
		errs = append(errs, errors.New("notify: Cloudflare API token is required"))
	}
	if from == "" {
		errs = append(errs, errors.New("notify: Cloudflare from address is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &cloudflareMailer{
		token:    token,
		from:     from,
		endpoint: cloudflareAPI + "/accounts/" + url.PathEscape(accountID) + "/email/sending/send",
		client:   &http.Client{Timeout: 15 * time.Second},
	}, nil
}

type cloudflareMailer struct {
	token    string
	from     string
	endpoint string
	client   *http.Client
}

func (m *cloudflareMailer) Send(ctx context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}

	payload, err := json.Marshal(map[string]any{
		"from":    m.from,
		"to":      []string{msg.To},
		"subject": msg.Subject,
		"text":    msg.TextBody,
	})
	if err != nil {
		return fmt.Errorf("notify: cloudflare encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint,
		bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("notify: cloudflare request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: cloudflare send: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Result *struct {
			PermanentBounces []string `json:"permanent_bounces"`
			Suppressed       []string `json:"suppressed_recipients"`
		} `json:"result"`
	}
	_ = json.Unmarshal(body, &out)

	if resp.StatusCode >= 300 || !out.Success {
		// Carry the API's own words: "403" alone gives an operator nothing to
		// act on, and this is the error they will actually see.
		var parts []string
		for _, e := range out.Errors {
			parts = append(parts, fmt.Sprintf("%s (%d)", e.Message, e.Code))
		}
		if len(parts) > 0 {
			return fmt.Errorf("notify: cloudflare rejected the message: %s", strings.Join(parts, "; "))
		}
		return fmt.Errorf("notify: cloudflare returned %s", resp.Status)
	}
	// Accepted is not sent: an address that bounced before, or that Cloudflare
	// suppresses, is answered with success and nothing goes out.
	if r := out.Result; r != nil && (len(r.PermanentBounces) > 0 || len(r.Suppressed) > 0) {
		return fmt.Errorf("notify: cloudflare did not send to %s: the address bounces or is suppressed", msg.To)
	}
	return nil
}
