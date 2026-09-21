package sysctl

import (
	"os"
	"path/filepath"
	"slices"
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

func readSetting(t *testing.T, root, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, strings.ReplaceAll(name, ".", "/")))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

func allOn() map[string]string {
	return map[string]string{
		"net.bridge.bridge-nf-call-iptables":  "1",
		"net.bridge.bridge-nf-call-ip6tables": "1",
		"net.ipv4.ip_forward":                 "1",
		"net.ipv6.conf.all.forwarding":        "1",
	}
}

func TestCheckPassesWhenBridgeSettingsAreOne(t *testing.T) {
	if err := Check(writeProcSys(t, allOn())); err != nil {
		t.Fatalf("Check returned %v, want nil", err)
	}
}

// Forwarding is the daemon's to turn on (EnsureForwarding), so Check does not
// refuse to start over it.
func TestCheckIgnoresForwarding(t *testing.T) {
	values := allOn()
	values["net.ipv4.ip_forward"] = "0"
	values["net.ipv6.conf.all.forwarding"] = "0"
	if err := Check(writeProcSys(t, values)); err != nil {
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

// br_netfilter belongs to the node's provisioning: Check reports a wrong bridge
// setting and leaves it as it is (ADR 0002).
func TestCheckDoesNotWriteBridgeSettings(t *testing.T) {
	values := allOn()
	values["net.bridge.bridge-nf-call-iptables"] = "0"
	root := writeProcSys(t, values)
	_ = Check(root)
	if got := readSetting(t, root, "net.bridge.bridge-nf-call-iptables"); got != "0" {
		t.Errorf("bridge-nf-call-iptables was changed to %q", got)
	}
}

func TestEnsureForwardingLeavesSettingsThatAreOne(t *testing.T) {
	root := writeProcSys(t, allOn())
	// Read-only files: a write would fail, so success means nothing was written.
	for _, name := range []string{"net/ipv4/ip_forward", "net/ipv6/conf/all/forwarding"} {
		if err := os.Chmod(filepath.Join(root, name), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := EnsureForwarding(root)
	if err != nil {
		t.Fatalf("EnsureForwarding returned %v, want nil", err)
	}
	if len(changed) != 0 {
		t.Errorf("EnsureForwarding changed %v, want nothing", changed)
	}
}

func TestEnsureForwardingTurnsOnWhatIsOff(t *testing.T) {
	values := allOn()
	values["net.ipv4.ip_forward"] = "0"
	values["net.ipv6.conf.all.forwarding"] = "0"
	root := writeProcSys(t, values)
	changed, err := EnsureForwarding(root)
	if err != nil {
		t.Fatalf("EnsureForwarding returned %v, want nil", err)
	}
	want := []string{"net.ipv4.ip_forward", "net.ipv6.conf.all.forwarding"}
	if !slices.Equal(changed, want) {
		t.Errorf("EnsureForwarding changed %v, want %v", changed, want)
	}
	for _, name := range want {
		if got := readSetting(t, root, name); got != "1" {
			t.Errorf("%s is %q after EnsureForwarding, want 1", name, got)
		}
	}
}

// A /proc/sys the daemon cannot write (mounted read-only, or no privilege) is a
// refusal to start, and the error says which file it tried to write.
func TestEnsureForwardingFailsWhenItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through file permissions")
	}
	values := allOn()
	values["net.ipv6.conf.all.forwarding"] = "0"
	root := writeProcSys(t, values)
	path := filepath.Join(root, "net/ipv6/conf/all/forwarding")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureForwarding(root)
	if err == nil {
		t.Fatal("EnsureForwarding returned nil, want an error")
	}
	for _, want := range []string{"net.ipv6.conf.all.forwarding", path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestEnsureForwardingDoesNotTouchBridgeSettings(t *testing.T) {
	values := allOn()
	values["net.bridge.bridge-nf-call-iptables"] = "0"
	values["net.ipv4.ip_forward"] = "0"
	root := writeProcSys(t, values)
	if _, err := EnsureForwarding(root); err != nil {
		t.Fatal(err)
	}
	if got := readSetting(t, root, "net.bridge.bridge-nf-call-iptables"); got != "0" {
		t.Errorf("bridge-nf-call-iptables was changed to %q", got)
	}
}
