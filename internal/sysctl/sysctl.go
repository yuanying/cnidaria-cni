// Package sysctl looks after the kernel settings the data plane needs (ADR 0002). It
// checks br_netfilter with bridge-nf-call-iptables and bridge-nf-call-ip6tables, which
// belong to the node's provisioning, and turns on IP forwarding for both families,
// which the daemon owns as the CNI it replaces did.
package sysctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProcSys is where the kernel exposes the settings on a real node.
const ProcSys = "/proc/sys"

// bridgeSettings only exist once br_netfilter is loaded, so their absence is the
// module missing, and the message says so.
var bridgeSettings = []string{
	"net.bridge.bridge-nf-call-iptables",
	"net.bridge.bridge-nf-call-ip6tables",
}

var forwardingSettings = []string{
	"net.ipv4.ip_forward",
	"net.ipv6.conf.all.forwarding",
}

// Check reads the br_netfilter settings under root (ProcSys on a node) and returns an
// error naming each one that is not 1, or nil when both are. It never writes: loading
// the module and setting these belongs to node provisioning (ADR 0002).
func Check(root string) error {
	var errs []error
	for _, name := range bridgeSettings {
		value, err := read(root, name)
		switch {
		case errors.Is(err, os.ErrNotExist):
			errs = append(errs, fmt.Errorf("%s is absent (is the br_netfilter module loaded?), must be 1", name))
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w, must be 1", name, err))
		case value != "1":
			errs = append(errs, fmt.Errorf("%s is %s, must be 1", name, value))
		}
	}
	return errors.Join(errs...)
}

// EnsureForwarding writes 1 to each forwarding setting under root that is not 1 and
// returns the names of those it changed. A setting it cannot read or write is an
// error naming the setting and the file.
func EnsureForwarding(root string) ([]string, error) {
	var changed []string
	for _, name := range forwardingSettings {
		value, err := read(root, name)
		if err != nil {
			return changed, fmt.Errorf("%s: %w", name, err)
		}
		if value == "1" {
			continue
		}
		if err := os.WriteFile(path(root, name), []byte("1\n"), 0o644); err != nil {
			return changed, fmt.Errorf("%s is %s and could not be set to 1: %w", name, value, err)
		}
		changed = append(changed, name)
	}
	return changed, nil
}

func path(root, name string) string {
	return filepath.Join(root, strings.ReplaceAll(name, ".", "/"))
}

func read(root, name string) (string, error) {
	raw, err := os.ReadFile(path(root, name))
	return strings.TrimSpace(string(raw)), err
}
