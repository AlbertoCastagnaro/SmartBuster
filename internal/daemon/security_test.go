package daemon_test

import (
	"testing"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/daemon"
)

// TestValidateBind is spec §8 DoD #4's remaining case: non-loopback bind
// refused without the explicit flag, allowed (with a warning) once given.
func TestValidateBind(t *testing.T) {
	cases := []struct {
		bind        string
		allowRemote bool
		wantErr     bool
		wantWarn    bool
	}{
		{"", false, false, false},
		{"127.0.0.1", false, false, false},
		{"localhost", false, false, false},
		{"::1", false, false, false},
		{"0.0.0.0", false, true, false},
		{"0.0.0.0", true, false, true},
		{"10.0.0.5", false, true, false},
		{"10.0.0.5", true, false, true},
	}
	for _, c := range cases {
		warn, err := daemon.ValidateBind(c.bind, c.allowRemote)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateBind(%q, %v): err=%v, wantErr=%v", c.bind, c.allowRemote, err, c.wantErr)
		}
		if warn != c.wantWarn {
			t.Errorf("ValidateBind(%q, %v): warn=%v, wantWarn=%v", c.bind, c.allowRemote, warn, c.wantWarn)
		}
	}
}
