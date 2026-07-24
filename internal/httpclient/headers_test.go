package httpclient

import "testing"

func TestBuildHeadersMinimalHasNoExtraHeaders(t *testing.T) {
	h := BuildHeaders(ProfileMinimal, "")
	if len(h) != 0 {
		t.Fatalf("expected minimal profile to add no headers, got %v", h)
	}
}

func TestBuildHeadersChromeSetAndValues(t *testing.T) {
	h := BuildHeaders(ProfileChrome, "")
	for _, key := range []string{"User-Agent", "Accept", "Accept-Language", "Accept-Encoding", "Sec-Fetch-Dest", "Sec-Ch-Ua", "Upgrade-Insecure-Requests"} {
		if h.Get(key) == "" {
			t.Errorf("expected chrome profile to set %s", key)
		}
	}
}

func TestBuildHeadersRefererSetOnlyWhenNonEmpty(t *testing.T) {
	h := BuildHeaders(ProfileChrome, "")
	if h.Get("Referer") != "" {
		t.Fatalf("expected no Referer when referer arg is empty")
	}
	h = BuildHeaders(ProfileChrome, "https://example.com/parent")
	if got := h.Get("Referer"); got != "https://example.com/parent" {
		t.Fatalf("expected Referer set, got %q", got)
	}
}

func TestBuildHeadersUnknownProfileFallsBackToMinimal(t *testing.T) {
	h := BuildHeaders("nonexistent", "")
	if len(h) != 0 {
		t.Fatalf("expected an unknown profile name to behave like minimal, got %v", h)
	}
}

func TestProfileStateStableUntilExplicitlyChanged(t *testing.T) {
	s := NewProfileState(ProfileChrome)
	for i := 0; i < 5; i++ {
		if got := s.Load(); got != ProfileChrome {
			t.Fatalf("expected profile to stay %q without an explicit Store, got %q", ProfileChrome, got)
		}
	}
	s.Store(ProfileFirefox)
	if got := s.Load(); got != ProfileFirefox {
		t.Fatalf("expected profile to change after an explicit Store, got %q", got)
	}
}
