package logrules

import (
	"strings"
	"testing"
)

func defaults(t *testing.T, rs *Ruleset, off ...string) *Ruleset {
	t.Helper()
	out, err := rs.WithDefaults(off)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDefaultMasks(t *testing.T) {
	rs := defaults(t, nil)
	cases := []struct{ in, want string }{
		{"paid with 4111 1111 1111 1111 ok", "paid with [REDACTED:card] ok"},
		{"card=4111-1111-1111-1111", "card=[REDACTED:card]"},
		{"card 5500005555555559 visa", "card [REDACTED:card] visa"},
		{"amex 378282246310005", "amex [REDACTED:card]"},
		// Fails the Luhn check: an order number, not a card.
		{"order 4111111111111112 shipped", "order 4111111111111112 shipped"},
		{"ts 1729339200123456", "ts 1729339200123456"},
		{"Authorization: Bearer abcDEF123456.xyz-_~+/==", "Authorization: Bearer [REDACTED]"},
		{"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U end", "token [REDACTED:jwt] end"},
		{"using key AKIAIOSFODNN7EXAMPLE now", "using key [REDACTED:aws-key] now"},
		{"aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "aws_secret_access_key=[REDACTED]"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nIBAAKC\n-----END RSA PRIVATE KEY-----\nafter", "[REDACTED:private-key]\nafter"},
		{"key: -----BEGIN OPENSSH PRIVATE KEY----- b3BlbnNzaC1rZXk", "key: [REDACTED:private-key]"},
		{"nothing to see", "nothing to see"},
	}
	for _, c := range cases {
		if got := rs.Mask(c.in); got != c.want {
			t.Errorf("Mask(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestDefaultMasksCanBeTurnedOff(t *testing.T) {
	in := "4111 1111 1111 1111 AKIAIOSFODNN7EXAMPLE"
	if got := defaults(t, nil, "all").Mask(in); got != in {
		t.Fatalf("all off still masked: %q", got)
	}
	if got := defaults(t, nil, MaskCard).Mask(in); got != "4111 1111 1111 1111 [REDACTED:aws-key]" {
		t.Fatalf("card off: %q", got)
	}
	if _, err := (*Ruleset)(nil).WithDefaults([]string{"cards"}); err == nil {
		t.Fatal("unknown mask name accepted")
	}
}

func TestUserRules(t *testing.T) {
	rs, err := Compile([]Rule{
		{Type: "exclude", Pattern: `GET /health`},
		{Type: "mask", Pattern: `(password=)\S+`, Replace: "${1}***"},
		{Type: "mask", Pattern: `\b\d{3}-\d{2}-\d{4}\b`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, keep := rs.Apply(`10.0.0.1 "GET /health HTTP/1.1" 200`); keep {
		t.Fatal("health check not excluded")
	}
	got, keep := rs.Apply("login user=bob password=hunter2 ssn 123-45-6789")
	if !keep || got != "login user=bob password=*** ssn [REDACTED]" {
		t.Fatalf("got %q", got)
	}
	if _, err := Compile([]Rule{{Type: "drop", Pattern: "x"}}); err == nil {
		t.Fatal("bad type accepted")
	}
	if _, err := Compile([]Rule{{Type: "mask", Pattern: "("}}); err == nil {
		t.Fatal("bad regex accepted")
	}
}

func TestParseMask(t *testing.T) {
	r, err := ParseMask(`[{"pattern":"secret=\\w+","replace":"secret=***"}]`)
	if err != nil || len(r) != 1 || r[0].Type != RuleMask || r[0].Replace != "secret=***" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = ParseMask(`{"pattern":"x"}`)
	if err != nil || len(r) != 1 {
		t.Fatalf("single object: %+v %v", r, err)
	}
	if _, err := ParseMask(`secret=\w+`); err == nil {
		t.Fatal("non-JSON accepted")
	}
}

func TestLuhn(t *testing.T) {
	for _, s := range []string{"4111111111111111", "4012888888881881", "6011111111111117", "3530111333300000"} {
		if !luhn(s) {
			t.Errorf("%s should pass", s)
		}
	}
	if luhn("4111111111111121") || luhn("1234") {
		t.Error("invalid numbers passed")
	}
	if !strings.Contains(cardRe.String(), "[2-6]") {
		t.Error("card regex changed")
	}
}
