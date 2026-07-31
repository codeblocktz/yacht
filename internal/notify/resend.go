package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const resendEndpoint = "https://api.resend.com/emails"

// NewResend returns a Mailer that sends through Resend's HTTP API.
//
// Chosen alongside SMTP because a self-hoster with no relay still needs mail,
// and an API key is less to get wrong than a relay. The HTTP call is written
// directly rather than pulling in an SDK: it is one POST, and a dependency that
// exists to save fifteen lines is a dependency to keep updated forever.
func NewResend(apiKey, from string) (Mailer, error) {
	var errs []error
	if apiKey == "" {
		errs = append(errs, errors.New("notify: Resend API key is required"))
	}
	if from == "" {
		errs = append(errs, errors.New("notify: Resend from address is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &resendMailer{
		apiKey:   apiKey,
		from:     from,
		endpoint: resendEndpoint,
		client:   &http.Client{Timeout: 15 * time.Second},
	}, nil
}

type resendMailer struct {
	apiKey   string
	from     string
	endpoint string
	client   *http.Client
}

func (m *resendMailer) Send(ctx context.Context, msg Message) error {
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
		return fmt.Errorf("notify: resend encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint,
		bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("notify: resend request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: resend send: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		// Carry the API's own message: "422" alone gives an operator nothing
		// to act on, and this is the error they will actually see.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Message != "" {
			return fmt.Errorf("notify: resend rejected the message: %s", apiErr.Message)
		}
		return fmt.Errorf("notify: resend returned %s", resp.Status)
	}
	return nil
}
