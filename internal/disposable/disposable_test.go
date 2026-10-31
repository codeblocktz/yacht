package disposable

import "testing"

func TestAddress(t *testing.T) {
	for _, tc := range []struct {
		email string
		want  bool
	}{
		{"someone@mailinator.com", true},
		{"Someone@MAILINATOR.com", true},
		// A subdomain of a disposable domain is as disposable.
		{"x@team.mailinator.com", true},
		{"x@mailinator.com.", true},
		{"someone@gmail.com", false},
		{"someone@datahuru.com", false},
		// A domain that only ends in the same letters is not a subdomain.
		{"someone@notreallymailinator.com", false},
		{"no-at-sign", false},
		{"", false},
	} {
		if got := Address(tc.email); got != tc.want {
			t.Errorf("Address(%q) = %v, want %v", tc.email, got, tc.want)
		}
	}
}

// A refresh that fetched an error page or nothing would quietly let every
// throwaway address through.
func TestTheListIsNotEmpty(t *testing.T) {
	if n := Len(); n < 1000 {
		t.Fatalf("the embedded list has %d domains, want thousands", n)
	}
}
