package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
)

// pdf is a stand-in for an invoice's PDF: bytes that are not text, so an
// encoding that mangles binary shows.
var pdf = append([]byte("%PDF-1.4\n"), 0x00, 0xff, 0x80, 0x0a, 0x0d, 0x7f)

func withPDF() Message {
	return Message{
		To: "amina@example.test", Subject: "Invoice CB-2026-000001", TextBody: "Your invoice.",
		Attachments: []Attachment{{Filename: "CB-2026-000001.pdf", ContentType: "application/pdf", Data: pdf}},
	}
}

func TestCloudflareSendsAttachmentsAsItsAPIAsks(t *testing.T) {
	var body struct {
		Attachments []map[string]string `json:"attachments"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":{}}`))
	}))
	defer srv.Close()
	m, _ := NewCloudflare("acct", "tok", "a@example.test", "")
	m.(*cloudflareMailer).endpoint = srv.URL
	if err := m.Send(context.Background(), withPDF()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(body.Attachments) != 1 {
		t.Fatalf("attachments = %v, want one", body.Attachments)
	}
	a := body.Attachments[0]
	if a["filename"] != "CB-2026-000001.pdf" || a["type"] != "application/pdf" || a["disposition"] != "attachment" {
		t.Errorf("attachment = %v", a)
	}
	if got, _ := base64.StdEncoding.DecodeString(a["content"]); !bytes.Equal(got, pdf) {
		t.Errorf("content decodes to %q, want %q", got, pdf)
	}
}

func TestCloudflareSendsNoAttachmentsFieldWithoutAttachments(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":{}}`))
	}))
	defer srv.Close()
	m, _ := NewCloudflare("acct", "tok", "a@example.test", "")
	m.(*cloudflareMailer).endpoint = srv.URL
	msg := withPDF()
	msg.Attachments = nil
	if err := m.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, ok := body["attachments"]; ok {
		t.Errorf("body has attachments: %v", body["attachments"])
	}
}

func TestResendSendsAttachmentsAsItsAPIAsks(t *testing.T) {
	var body struct {
		Attachments []map[string]string `json:"attachments"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		_, _ = w.Write([]byte(`{"id":"abc"}`))
	}))
	defer srv.Close()
	m, _ := NewResend("re_123", "a@example.test")
	m.(*resendMailer).endpoint = srv.URL
	if err := m.Send(context.Background(), withPDF()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(body.Attachments) != 1 {
		t.Fatalf("attachments = %v, want one", body.Attachments)
	}
	a := body.Attachments[0]
	if a["filename"] != "CB-2026-000001.pdf" || a["content_type"] != "application/pdf" {
		t.Errorf("attachment = %v", a)
	}
	if got, _ := base64.StdEncoding.DecodeString(a["content"]); !bytes.Equal(got, pdf) {
		t.Errorf("content decodes to %q, want %q", got, pdf)
	}
}

// Over SMTP the mail is multipart/mixed: the text and HTML as one
// multipart/alternative, then the file in base64, as a mail client parses it.
func TestSMTPSendsAttachmentsAsMultipartMixed(t *testing.T) {
	relay := newRelay(t)
	m, err := NewSMTP(SMTPConfig{Addr: relay.addr, From: "Acme <no-reply@example.test>"})
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}
	if err := m.Send(context.Background(), withPDF()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := <-relay.got
	msg, err := mail.ReadMessage(strings.NewReader(got.data))
	if err != nil {
		t.Fatalf("not a mail: %v\n%s", err, got.data)
	}
	kind, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if kind != "multipart/mixed" {
		t.Fatalf("Content-Type = %s, want multipart/mixed", kind)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var parts []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("part: %v", err)
		}
		kind, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		parts = append(parts, kind)
		if kind != "application/pdf" {
			continue
		}
		if cd := p.Header.Get("Content-Disposition"); cd != `attachment; filename="CB-2026-000001.pdf"` {
			t.Errorf("Content-Disposition = %q", cd)
		}
		if te := p.Header.Get("Content-Transfer-Encoding"); te != "base64" {
			t.Errorf("Content-Transfer-Encoding = %q", te)
		}
		// multipart.Part leaves base64 to the reader, as a client does.
		raw, _ := io.ReadAll(p)
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\r\n") {
			if len(line) > 76 {
				t.Errorf("a base64 line of %d characters", len(line))
			}
		}
		dec, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\r", "", "\n", "").Replace(string(raw)))
		if err != nil || !bytes.Equal(dec, pdf) {
			t.Errorf("attachment decodes to %q (%v), want %q", dec, err, pdf)
		}
	}
	if strings.Join(parts, ",") != "multipart/alternative,application/pdf" {
		t.Errorf("parts = %v, want the alternative then the PDF", parts)
	}
}

func TestAttachmentsAreValidated(t *testing.T) {
	for _, a := range []Attachment{
		{Filename: "", Data: pdf},
		{Filename: "a\r\nBcc: x@y.test", Data: pdf},
		{Filename: `a".pdf`, Data: pdf},
		{Filename: "../a.pdf", Data: pdf},
		{Filename: "a.pdf"},
		{Filename: "a.pdf", Data: make([]byte, MaxAttachmentBytes+1)},
	} {
		msg := withPDF()
		msg.Attachments = []Attachment{a}
		if err := msg.Validate(); err == nil {
			t.Errorf("Validate accepted the attachment %q (%d bytes)", a.Filename, len(a.Data))
		}
	}
	if err := withPDF().Validate(); err != nil {
		t.Errorf("Validate refused a PDF: %v", err)
	}
}
