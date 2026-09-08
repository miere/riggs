package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// A config that never mentions jobs still bounds them. The setting is an
// override, and "not configured" is by far the common case.
func TestJobTimeoutsHaveDefaults(t *testing.T) {
	cfg := loadConfig(t, "admin:\n  slack-user-id: U1\n")
	for _, id := range []JobTimeoutID{JobTimeoutGitHub, JobTimeoutJira} {
		if got := cfg.JobTimeout(id); got != DefaultJobTimeout {
			t.Errorf("%s = %v, want %v", id, got, DefaultJobTimeout)
		}
		if cfg.JobTimeoutOverridden(id) {
			t.Errorf("%s reads as configured on an untouched config", id)
		}
	}
	// The modal is pre-filled with what is IN FORCE, not with an empty box:
	// "empty means two minutes" is a distinction the writer needs and the
	// reader does not.
	if got := cfg.JobTimeoutText(JobTimeoutJira); got != "2m0s" {
		t.Errorf("prefill = %q, want the effective bound", got)
	}
}

// The whole reason the setting exists: one kind can be given more room without
// touching the other.
func TestJobTimeoutsAreReadPerKind(t *testing.T) {
	cfg := loadConfig(t, "jobs:\n  jira-timeout: 10m\n")

	if got := cfg.JobTimeout(JobTimeoutJira); got != 10*time.Minute {
		t.Errorf("jira = %v, want 10m", got)
	}
	if got := cfg.JobTimeout(JobTimeoutGitHub); got != DefaultJobTimeout {
		t.Errorf("github = %v, want the default", got)
	}
	if !cfg.JobTimeoutOverridden(JobTimeoutJira) {
		t.Error("a configured timeout does not read as configured")
	}
}

// A setting that will not parse resolves to the DEFAULT, never to zero. The
// scheduler is about to run something and has nobody to tell; the two readings
// of a broken value are "no timeout at all" and "the default", and only one of
// them lets a job run until the daemon is restarted.
func TestABrokenJobTimeoutFallsBackRatherThanToZero(t *testing.T) {
	for name, yaml := range map[string]string{
		"not a duration": "jobs:\n  jira-timeout: soon\n",
		"zero":           "jobs:\n  jira-timeout: 0s\n",
		"negative":       "jobs:\n  jira-timeout: -5m\n",
		"absurd":         "jobs:\n  jira-timeout: 3h\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := loadConfig(t, yaml)
			if got := cfg.JobTimeout(JobTimeoutJira); got != DefaultJobTimeout {
				t.Fatalf("timeout = %v, want the default", got)
			}
		})
	}
}

// Refused at the modal, where a human is standing — which is the difference
// between "45x is not a duration" and a digest that silently stops working.
func TestValidateJobTimeout(t *testing.T) {
	for name, tc := range map[string]struct{ raw, want string }{
		"empty is a reset": {"", ""},
		"a duration":       {"90s", ""},
		"nonsense":         {"soon", "not a duration"},
		"zero":             {"0s", "needs some time"},
		"negative":         {"-1m", "needs some time"},
		"longer than an hour": {"90m",
			"is a service, not a job"},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateJobTimeout(tc.raw)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateJobTimeout(%q) = %v", tc.raw, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The write path, and the proof that matters: the file loads again afterwards.
func TestSetJobTimeoutRoundTrips(t *testing.T) {
	cfg := loadConfig(t, "admin:\n  slack-user-id: U1\n")

	if err := cfg.SetJobTimeout(JobTimeoutGitHub, "5m"); err != nil {
		t.Fatalf("SetJobTimeout: %v", err)
	}
	if got := cfg.JobTimeout(JobTimeoutGitHub); got != 5*time.Minute {
		t.Fatalf("the in-memory value did not change: %v", got)
	}
	reloaded, err := Load(cfg.Path)
	if err != nil {
		t.Fatalf("re-loading after a timeout save: %v", err)
	}
	if got := reloaded.JobTimeout(JobTimeoutGitHub); got != 5*time.Minute {
		t.Fatalf("the saved value did not survive a reload: %v", got)
	}

	// An invalid one is refused rather than written, so the file cannot be left
	// carrying a bound that silently does not apply.
	if err := cfg.SetJobTimeout(JobTimeoutGitHub, "soon"); err == nil {
		t.Fatal("SetJobTimeout accepted a value that is not a duration")
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "soon") {
		t.Fatalf("the rejected value was written anyway:\n%s", raw)
	}
}

// An empty value RESETS: the key is deleted rather than filled with today's
// default, so a later change to the default reaches this machine.
func TestClearingAJobTimeoutDeletesTheKey(t *testing.T) {
	cfg := loadConfig(t, "jobs:\n  jira-timeout: 10m\n")

	if err := cfg.SetJobTimeout(JobTimeoutJira, ""); err != nil {
		t.Fatalf("SetJobTimeout: %v", err)
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "jira-timeout") {
		t.Fatalf("the key survived the reset:\n%s", raw)
	}
	if cfg.JobTimeoutOverridden(JobTimeoutJira) {
		t.Error("a reset timeout still reads as configured")
	}
}
