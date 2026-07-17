package scope

import "testing"

func TestExactHostAllow(t *testing.T) {
	s, err := New(Config{AllowHosts: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if !s.InScope("https://example.com/admin") {
		t.Fatal("expected in scope")
	}
	if s.InScope("https://evil.com/admin") {
		t.Fatal("expected out of scope")
	}
}

func TestWildcardHostAllow(t *testing.T) {
	s, err := New(Config{AllowHosts: []string{"*.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if !s.InScope("https://api.example.com/x") {
		t.Fatal("expected subdomain in scope")
	}
	if s.InScope("https://example.com/x") {
		t.Fatal("expected bare apex domain NOT matched by *.example.com")
	}
	if s.InScope("https://notexample.com/x") {
		t.Fatal("expected lookalike domain out of scope")
	}
}

func TestCIDRHostAllow(t *testing.T) {
	s, err := New(Config{AllowHosts: []string{"10.0.0.0/24"}})
	if err != nil {
		t.Fatal(err)
	}
	if !s.InScope("http://10.0.0.5/x") {
		t.Fatal("expected IP within CIDR to be in scope")
	}
	if s.InScope("http://10.0.1.5/x") {
		t.Fatal("expected IP outside CIDR to be out of scope")
	}
}

func TestExcludeTakesPrecedenceOverAllow(t *testing.T) {
	s, err := New(Config{
		AllowHosts:   []string{"*.example.com"},
		ExcludeHosts: []string{"internal.example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.InScope("https://internal.example.com/x") {
		t.Fatal("expected explicit exclude to override wildcard allow")
	}
	if !s.InScope("https://api.example.com/x") {
		t.Fatal("expected other subdomain to remain in scope")
	}
}

func TestExcludePathPattern(t *testing.T) {
	s, err := New(Config{ExcludePatterns: []string{`^/logout`}})
	if err != nil {
		t.Fatal(err)
	}
	if s.InScope("https://example.com/logout") {
		t.Fatal("expected excluded path pattern to block")
	}
	if !s.InScope("https://example.com/login") {
		t.Fatal("expected non-matching path to be in scope")
	}
}

func TestEmptyAllowlistAllowsAnyNonExcluded(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !s.InScope("https://anything.example/x") {
		t.Fatal("expected empty allowlist to allow by default")
	}
}

func TestInvalidURLIsOutOfScope(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if s.InScope("http://[::1") {
		t.Fatal("expected malformed URL to be rejected")
	}
}

func TestInvalidExcludePatternErrors(t *testing.T) {
	_, err := New(Config{ExcludePatterns: []string{"("}})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}
