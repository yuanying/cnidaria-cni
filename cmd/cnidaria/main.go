// Command cnidaria is the node daemon. One copy runs on every node (a DaemonSet with
// hostNetwork) and looks after that node only: it writes the CNI conflist, keeps the
// host-gw routes to the other nodes, keeps pod traffic past an iptables FORWARD policy
// of DROP, and renders NetworkPolicy and NodeNetworkPolicy into the node's nftables
// table (ADR 0001, 0003, 0007).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/yuanying/cnidaria-cni/internal/apis/v1alpha1"
	"github.com/yuanying/cnidaria-cni/internal/controller"
	"github.com/yuanying/cnidaria-cni/internal/iptables"
	"github.com/yuanying/cnidaria-cni/internal/nftables"
	"github.com/yuanying/cnidaria-cni/internal/routes"
	"github.com/yuanying/cnidaria-cni/internal/sysctl"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cnidaria:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		nodeName     = flag.String("node-name", os.Getenv("NODE_NAME"), "Name of the Node this daemon runs on. Defaults to $NODE_NAME.")
		conflistPath = flag.String("conflist", "/etc/cni/net.d/10-cnidaria.conflist", "Where to write the CNI configuration list.")
		mtu          = flag.Int("mtu", 0, "MTU for the bridge and the pod interfaces. 0 reads it from the interface holding the node's InternalIP.")
		networkName  = flag.String("network-name", "cnidaria", "Network name in the conflist. host-local keeps its leases under /var/lib/cni/networks/<name>, so set it to the previous CNI's network name when migrating (ADR 0009).")
		healthAddr   = flag.String("health-addr", "127.0.0.1:19080", "Address for the /healthz and /readyz probes.")
		metricsAddr  = flag.String("metrics-addr", "0", "Address for Prometheus metrics. 0 disables them.")
		procSys      = flag.String("proc-sys", sysctl.ProcSys, "Where the kernel settings are read and written. In a container, a writable mount of the host's /proc/sys/net under a /proc/sys-shaped path (ADR 0002).")
		zapOpts      zap.Options
	)
	ctrl.RegisterFlags(flag.CommandLine)
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	if *nodeName == "" {
		return fmt.Errorf("--node-name or NODE_NAME is required")
	}
	// Refusing to start is the point (ADR 0002): a warning would scroll away and
	// the policy would silently miss half the traffic.
	if err := sysctl.Check(*procSys); err != nil {
		return fmt.Errorf("kernel settings the data plane needs are not in place:\n%w", err)
	}
	// Forwarding is ours to turn on, as the CNI this replaces did at run time: a
	// node whose provisioning never persisted it comes back from a reboot with it
	// off (ADR 0002).
	changed, err := sysctl.EnsureForwarding(*procSys)
	for _, name := range changed {
		ctrl.Log.Info("Turned on forwarding", "sysctl", name)
	}
	if err != nil {
		return fmt.Errorf("could not turn on forwarding:\n%w", err)
	}

	// One manager, no leader election (each node acts for itself), probes and
	// metrics on localhost only (ADR 0007). managedFields are stripped from every
	// cached object to keep memory down on small nodes.
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	// The core types and the NodeNetworkPolicy CRD are read through one scheme (ADR 0007).
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			return fmt.Errorf("scheme: %w", err)
		}
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultTransform: cache.TransformStripManagedFields(),
			// Every node holds every pod: a policy peer may name one anywhere
			// (ADR 0007), so a pod is cut down to what the renderer reads.
			ByObject: map[client.Object]cache.ByObject{&corev1.Pod{}: {Transform: controller.StripPod}},
		},
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *healthAddr,
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}

	// The forward chain goes through whichever iptables backend kube-proxy uses
	// on this node; without iptables at all, a FORWARD policy of DROP would take
	// the pod network down, so that is a refusal to start (ADR 0003).
	forward, err := iptables.Detect(context.Background(), iptables.Exec{})
	if err != nil {
		return err
	}
	ctrl.Log.Info("Chose the iptables backends for the forward chain",
		"iptables", forward.V4, "iptablesReason", forward.V4Reason,
		"ip6tables", forward.V6, "ip6tablesReason", forward.V6Reason)

	kernel, err := routes.NewKernel()
	if err != nil {
		return err
	}
	defer kernel.Close()
	r := &controller.Routes{
		Client:   mgr.GetClient(),
		NodeName: *nodeName,
		Kernel:   kernel,
		Forward:  forward,
		Conflist: controller.Conflist{
			Path:   *conflistPath,
			Name:   *networkName,
			Bridge: "cni0",
			MTU:    *mtu,
		},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("routes reconciler: %w", err)
	}
	ruleset := &controller.Ruleset{
		Client:    mgr.GetClient(),
		NodeName:  *nodeName,
		SafePorts: nftables.DefaultSafePorts,
		Applier:   nftables.NewApplier(nftables.NFT{}),
	}
	if err := ruleset.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("ruleset reconciler: %w", err)
	}

	return mgr.Start(ctrl.SetupSignalHandler())
}
