package k8s

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// A custom domain gets a TLS entry of its own, naming a Secret in the app's
// namespace, and the Ingress asks cert-manager to fill it. The wildcard's entry
// beside it still names none.
func TestIssuedHostsGetTheirOwnCertificate(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com", "shop.customer.test", "www.customer.test"}
	spec.TLSHosts = []string{"web.apps.example.com"}
	spec.IssuedHosts = []string{"shop.customer.test", "www.customer.test"}
	spec.CertIssuer = "yacht-acme"

	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}

	if got := ing.Annotations[certManagerClusterIssuer]; got != "yacht-acme" {
		t.Errorf("cluster-issuer annotation = %q, want yacht-acme", got)
	}
	if len(ing.Spec.TLS) != 3 {
		t.Fatalf("tls = %+v, want the wildcard entry and one per issued host", ing.Spec.TLS)
	}
	if ing.Spec.TLS[0].SecretName != "" {
		t.Errorf("wildcard entry names secret %q, want none", ing.Spec.TLS[0].SecretName)
	}
	// One entry per host: a certificate is issued whole or not at all, so one
	// drifted domain must not hold back the other.
	for i, host := range spec.IssuedHosts {
		entry := ing.Spec.TLS[i+1]
		if len(entry.Hosts) != 1 || entry.Hosts[0] != host {
			t.Errorf("entry %d hosts = %v, want [%s]", i+1, entry.Hosts, host)
		}
		if entry.SecretName != orchestrator.CertSecretName(host) {
			t.Errorf("entry %d secret = %q, want %q", i+1, entry.SecretName, orchestrator.CertSecretName(host))
		}
	}
}

// Nothing to issue, nothing asked of cert-manager: an install without it gets
// no annotation for software that is not there.
func TestNoIssuedHostsLeavesCertManagerOut(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"shop.customer.test"}
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	if _, ok := ing.Annotations[certManagerClusterIssuer]; ok {
		t.Error("cluster-issuer annotation written with nothing to issue")
	}
	if len(ing.Spec.TLS) != 0 {
		t.Errorf("tls = %+v, want none", ing.Spec.TLS)
	}
}

func TestIssuedHostsMustBeRoutedAndOutsideTheWildcard(t *testing.T) {
	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com", "shop.customer.test"}
	spec.TLSHosts = []string{"web.apps.example.com"}
	spec.CertIssuer = "yacht-acme"

	for name, issued := range map[string][]string{
		"not routed":       {"other.customer.test"},
		"already wildcard": {"web.apps.example.com"},
	} {
		spec.IssuedHosts = issued
		if err := spec.Validate(); err == nil {
			t.Errorf("%s: issuing %v validated", name, issued)
		}
	}

	spec.IssuedHosts = []string{"shop.customer.test"}
	spec.CertIssuer = ""
	if err := spec.Validate(); err == nil {
		t.Error("issued hosts with no issuer validated")
	}
}

func TestCertSecretNameFitsALabel(t *testing.T) {
	if got := orchestrator.CertSecretName("shop.customer.test"); got != "tls-shop.customer.test" {
		t.Errorf("short host = %q, want the host itself, readably", got)
	}

	long := strings.Repeat("a", 60) + ".customer.test"
	other := strings.Repeat("a", 60) + ".customer.other"
	a, b := orchestrator.CertSecretName(long), orchestrator.CertSecretName(other)
	if len(a) > 63 || len(b) > 63 {
		t.Errorf("long names = %d and %d characters, want at most 63", len(a), len(b))
	}
	if a == b {
		t.Errorf("two hosts sharing a prefix got the same secret %q", a)
	}
	if strings.HasSuffix(a, "-") || strings.Contains(a, ".-") {
		t.Errorf("shortened name %q is not a valid object name", a)
	}
}

// What the dashboard shows comes from the Secret: absent is still being
// issued, a certificate for the host is issued, and one for some other name is
// not this host's certificate.
func TestCertificateIsReadFromTheSecret(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)
	ref := testSpec().Ref
	host := "shop.customer.test"

	cert, err := o.Certificate(ctx, ref, host)
	if err != nil || cert.Issued {
		t.Fatalf("no secret = %+v, %v; want not issued and no error", cert, err)
	}

	notAfter := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	putCert(t, client.CoreV1().Secrets(ref.Namespace), host, "someone-else.test", notAfter)
	if cert, err := o.Certificate(ctx, ref, host); err != nil || cert.Issued {
		t.Fatalf("certificate for another name = %+v, %v; want not issued", cert, err)
	}

	putCert(t, client.CoreV1().Secrets(ref.Namespace), host, host, notAfter)
	cert, err = o.Certificate(ctx, ref, host)
	if err != nil {
		t.Fatalf("Certificate: %v", err)
	}
	if !cert.Issued || !cert.NotAfter.Equal(notAfter) {
		t.Fatalf("certificate = %+v, want issued until %v", cert, notAfter)
	}
	// Self-signed, as a staging certificate effectively is: issued, and not
	// something a browser accepts.
	if cert.Trusted {
		t.Error("a certificate chaining to no trusted root is reported trusted")
	}
}

type secretWriter interface {
	Get(context.Context, string, metav1.GetOptions) (*corev1.Secret, error)
	Create(context.Context, *corev1.Secret, metav1.CreateOptions) (*corev1.Secret, error)
	Update(context.Context, *corev1.Secret, metav1.UpdateOptions) (*corev1.Secret, error)
}

// putCert writes the Secret cert-manager would, holding a certificate for
// subject, under the name the certificate for host is kept in.
func putCert(t *testing.T, secrets secretWriter, host, subject string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: subject},
		DNSNames:     []string{subject},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: orchestrator.CertSecretName(host)},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		},
	}
	ctx := context.Background()
	if _, err := secrets.Get(ctx, sec.Name, metav1.GetOptions{}); err == nil {
		_, err = secrets.Update(ctx, sec, metav1.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A domain removed from the app stops being asked for. Server-side apply drops
// what the next apply does not repeat, so the annotation and the entry go with
// the last issued host — and cert-manager deletes the Certificate it no longer
// has an entry for.
func TestIssuanceStopsWhenTheLastIssuedHostGoes(t *testing.T) {
	ctx := context.Background()
	o, client := testOrchestrator(t)

	spec := testSpec()
	spec.Hosts = []string{"web.apps.example.com", "shop.customer.test"}
	spec.IssuedHosts = []string{"shop.customer.test"}
	spec.CertIssuer = "yacht-acme"
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}

	spec.Hosts = []string{"web.apps.example.com"}
	spec.IssuedHosts = nil
	if err := o.ApplyApp(ctx, spec); err != nil {
		t.Fatalf("ApplyApp: %v", err)
	}
	ing, err := client.NetworkingV1().Ingresses(spec.Namespace).
		Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	if _, ok := ing.Annotations[certManagerClusterIssuer]; ok {
		t.Error("cluster-issuer annotation outlived the last issued host")
	}
	if len(ing.Spec.TLS) != 0 {
		t.Errorf("tls = %+v, want none", ing.Spec.TLS)
	}
}
