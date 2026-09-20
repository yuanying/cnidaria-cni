package sysctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProcSys lays out a fake /proc/sys tree with the given values. A setting whose
// value is "" is left absent, which is what an unloaded module looks like.
func writeProcSys(t *testing.T, values map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, value := range values {
		if value == "" {
			continue
		}
		path := filepath.Join(root, strings.ReplaceAll(name, ".", "/"))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func allOn() map[string]string {
	return map[string]string{
		"net.bridge.bridge-nf-call-iptables":  "1",
		"net.bridge.bridge-nf-call-ip6tables": "1",
		"net.ipv4.ip_forward":                 "1",
		"net.ipv6.conf.all.forwarding":        "1",
	}
}

func TestCheckPassesWhenEverythingIsOne(t *testing.T) {
	if err := Check(writeProcSys(t, allOn())); err != nil {
		t.Fatalf("Check returned %v, want nil", err)
	}
}

// The error has to tell the operator which sysctl to fix and what it should be
// (ADR 0002): a daemon that refuses to start owes the reason.
func TestCheckNamesEverySettingThatIsWrong(t *testing.T) {
	cases := []struct {
		name     string
		override map[string]string
		wantIn   []string
	}{
		{
			name:     "bridge-nf-call-iptables is 0",
			override: map[string]string{"net.bridge.bridge-nf-call-iptables": "0"},
			wantIn:   []string{"net.bridge.bridge-nf-call-iptables", "is 0", "must be 1"},
		},
		{
			name:     "bridge-nf-call-ip6tables is 0",
			override: map[string]string{"net.bridge.bridge-nf-call-ip6tables": "0"},
			wantIn:   []string{"net.bridge.bridge-nf-call-ip6tables", "is 0", "must be 1"},
		},
		{
			name:     "ip_forward is 0",
			override: map[string]string{"net.ipv4.ip_forward": "0"},
			wantIn:   []string{"net.ipv4.ip_forward", "is 0", "must be 1"},
		},
		{
			name:     "ipv6 forwarding is 0",
			override: map[string]string{"net.ipv6.conf.all.forwarding": "0"},
			wantIn:   []string{"net.ipv6.conf.all.forwarding", "is 0", "must be 1"},
		},
		{
			// br_netfilter not loaded: the files do not exist at all.
			name: "bridge sysctls are absent",
			override: map[string]string{
				"net.bridge.bridge-nf-call-iptables":  "",
				"net.bridge.bridge-nf-call-ip6tables": "",
			},
			wantIn: []string{
				"net.bridge.bridge-nf-call-iptables", "net.bridge.bridge-nf-call-ip6tables",
				"br_netfilter", "must be 1",
			},
		},
		{
			name: "two settings wrong at once are both reported",
			override: map[string]string{
				"net.ipv4.ip_forward":          "0",
				"net.ipv6.conf.all.forwarding": "0",
			},
			wantIn: []string{"net.ipv4.ip_forward", "net.ipv6.conf.all.forwarding"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := allOn()
			for k, v := range tc.override {
				values[k] = v
			}
			err := Check(writeProcSys(t, values))
			if err == nil {
				t.Fatal("Check returned nil, want an error")
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// Check reads and never writes: a daemon that quietly changes kernel-wide settings is
// harder to reason about than one that says what it needs (ADR 0002).
func TestCheckDoesNotModifyAnything(t *testing.T) {
	values := allOn()
	values["net.ipv4.ip_forward"] = "0"
	root := writeProcSys(t, values)
	_ = Check(root)
	got, err := os.ReadFile(filepath.Join(root, "net/ipv4/ip_forward"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "0" {
		t.Errorf("ip_forward was changed to %q", got)
	}
}
