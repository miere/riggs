package app

import "testing"

func TestDaemonProfileParsing(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"absent":   {nil, ""},
		"spaced":   {[]string{"--slack-profile", "riggs"}, "riggs"},
		"appended": {[]string{"--slack-profile=riggs"}, "riggs"},
	}
	for name, tc := range cases {
		got, err := daemonProfile(tc.args)
		if err != nil {
			t.Fatalf("%s: daemonProfile: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: daemonProfile = %q, want %q", name, got, tc.want)
		}
	}
}

// A mistyped flag must not silently start a daemon listening as the wrong app.
func TestDaemonProfileRejectsBadArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"missing value": {"--slack-profile"},
		"empty value":   {"--slack-profile="},
		"stray token":   {"riggs"},
		"unknown flag":  {"--slack-channel", "C1"},
	} {
		if got, err := daemonProfile(args); err == nil {
			t.Errorf("%s: daemonProfile returned %q, want an error", name, got)
		}
	}
}
