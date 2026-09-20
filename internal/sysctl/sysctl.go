// Package sysctl reads the kernel settings cnidaria depends on and refuses to start
// when they are not what the data plane needs: br_netfilter with
// bridge-nf-call-iptables and bridge-nf-call-ip6tables set (ADR 0002) and IP
// forwarding for both families.
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

// required lists every setting the data plane needs and the value it must have.
// The first two only exist once br_netfilter is loaded, so their absence is the
// module missing, and the message says so.
var required = []struct {
	name   string
	module string // the module that provides the setting, when it is not built in
}{
	{name: "net.bridge.bridge-nf-call-iptables", module: "br_netfilter"},
	{name: "net.bridge.bridge-nf-call-ip6tables", module: "br_netfilter"},
	{name: "net.ipv4.ip_forward"},
	{name: "net.ipv6.conf.all.forwarding"},
}

// Check reads every required setting under root (ProcSys on a node) and returns an
// error naming each one that is not 1, or nil when all of them are. It never writes:
// fixing the settings belongs to node provisioning, not to the daemon (ADR 0002).
func Check(root string) error {
	var errs []error
	for _, s := range required {
		path := filepath.Join(root, strings.ReplaceAll(s.name, ".", "/"))
		raw, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist) && s.module != "":
			errs = append(errs, fmt.Errorf("%s is absent (is the %s module loaded?), must be 1", s.name, s.module))
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w, must be 1", s.name, err))
		case strings.TrimSpace(string(raw)) != "1":
			errs = append(errs, fmt.Errorf("%s is %s, must be 1", s.name, strings.TrimSpace(string(raw))))
		}
	}
	return errors.Join(errs...)
}
