package testbed

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netns"
)

// Outcome is how a connection attempt from a pod ended. A packet that a rule dropped
// produces nothing at all, so the attempt runs out of time, while a port nobody
// listens on answers with a reset at once. That difference in timing is how a test
// tells "denied" from "nothing is listening" (ADR 0008).
type Outcome string

// The ways a connection attempt ends.
const (
	Open        Outcome = "open"
	Refused     Outcome = "refused"
	Dropped     Outcome = "dropped"
	Unreachable Outcome = "unreachable"
)

// Serve accepts TCP connections on the port inside the pod's namespace until the test
// ends, on both address families. What it accepts it closes again: the tests ask
// whether a connection can be made, not what travels over it.
func (p *Pod) Serve(t testing.TB, port uint16) {
	t.Helper()
	serve(t, p.NS, "pod "+p.Name, port)
}

// Serve on a node listens in the node's own namespace, which is where a NodePolicy
// decides what may arrive (ADR 0004).
func (n *Node) Serve(t testing.TB, port uint16) {
	t.Helper()
	serve(t, n.NS, "node "+n.Name, port)
}

func serve(t testing.TB, ns, who string, port uint16) {
	t.Helper()
	var listener net.Listener
	if err := inNamespace(ns, func() error {
		var err error
		listener, err = net.Listen("tcp", fmt.Sprintf(":%d", port))
		return err
	}); err != nil {
		t.Fatalf("listening on port %d in %s: %v", port, who, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
}

// Connect makes one TCP connection from the pod's namespace and reports how it ended.
func (p *Pod) Connect(t testing.TB, addr netip.Addr, port uint16, timeout time.Duration) Outcome {
	t.Helper()
	return connect(t, p.NS, "pod "+p.Name, addr, port, timeout)
}

// Connect from a node is the same from the node's own namespace: the end a NodePolicy
// governs.
func (n *Node) Connect(t testing.TB, addr netip.Addr, port uint16, timeout time.Duration) Outcome {
	t.Helper()
	return connect(t, n.NS, "node "+n.Name, addr, port, timeout)
}

func connect(t testing.TB, ns, who string, addr netip.Addr, port uint16, timeout time.Duration) Outcome {
	t.Helper()
	target := netip.AddrPortFrom(addr, port).String()
	var outcome Outcome
	err := inNamespace(ns, func() error {
		conn, err := net.DialTimeout("tcp", target, timeout)
		switch {
		case err == nil:
			_ = conn.Close()
			outcome = Open
		case errors.Is(err, syscall.ECONNREFUSED):
			outcome = Refused
		case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
			outcome = Unreachable
		case os.IsTimeout(err):
			outcome = Dropped
		default:
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("connecting from %s to %s: %v", who, target, err)
	}
	return outcome
}

// inNamespace runs fn on a thread that has been moved into the namespace and moves
// the thread back afterwards. A socket fn opens belongs to that namespace from then
// on, wherever it is later read from, which is what lets a test hold a listener in a
// pod while the test itself runs outside it.
func inNamespace(name string, fn func() error) (err error) {
	runtime.LockOSThread()
	unlock := true
	defer func() {
		if unlock {
			runtime.UnlockOSThread()
		}
	}()

	here, err := netns.Get()
	if err != nil {
		return fmt.Errorf("reading the current namespace: %w", err)
	}
	defer func() { _ = here.Close() }()
	there, err := netns.GetFromName(name)
	if err != nil {
		return fmt.Errorf("opening namespace %s: %w", name, err)
	}
	defer func() { _ = there.Close() }()

	if err := netns.Set(there); err != nil {
		return fmt.Errorf("entering namespace %s: %w", name, err)
	}
	defer func() {
		if back := netns.Set(here); back != nil {
			// The thread is still in the wrong namespace, so it must not go back
			// to the runtime's pool. Leaving it locked keeps every other
			// goroutine off it.
			unlock = false
			err = errors.Join(err, fmt.Errorf("leaving namespace %s: %w", name, back))
		}
	}()
	return fn()
}
