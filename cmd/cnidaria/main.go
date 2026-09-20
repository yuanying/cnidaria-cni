// Command cnidaria is the node daemon. One copy runs on every node (a DaemonSet with
// hostNetwork) and looks after that node only: it writes the CNI conflist, keeps the
// host-gw routes to the other nodes, and renders NetworkPolicy and NodePolicy into
// the node's nftables table (ADR 0001, 0003, 0007).
package main

import (
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
		nodeName      = flag.String("node-name", os.Getenv("NODE_NAME"), "Name of the Node this daemon runs on. Defaults to $NODE_NAME.")
		conflistPath  = flag.String("conflist", "/etc/cni/net.d/10-cnidaria.conflist", "Where to write the CNI configuration list.")
		mtu           = flag.Int("mtu", 0, "MTU for the bridge and the pod interfaces. 0 reads it from the interface holding the node's InternalIP.")
		ipamStoreName = flag.String("ipam-store-name", "", "Name of host-local's lease directory under /var/lib/cni/networks. Empty uses the network name. Set to the previous CNI's network name when migrating (ADR 0009).")
		healthAddr    = flag.String("health-addr", "127.0.0.1:19080", "Address for the /healthz and /readyz probes.")
		metricsAddr   = flag.String("metrics-addr", "0", "Address for Prometheus metrics. 0 disables them.")
		zapOpts       zap.Options
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
	if err := sysctl.Check(sysctl.ProcSys); err != nil {
		return fmt.Errorf("kernel settings the data plane needs are not in place:\n%w", err)
	}

	// One manager, no leader election (each node acts for itself), probes and
	// metrics on localhost only (ADR 0007). managedFields are stripped from every
	// cached object to keep memory down on small nodes.
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	// The core types and the NodePolicy CRD are read through one scheme (ADR 0007).
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

	kernel, err := routes.NewKernel()
	if err != nil {
		return err
	}
	defer kernel.Close()
	r := &controller.Routes{
		Reader:   mgr.GetCache(),
		NodeName: *nodeName,
		Kernel:   kernel,
		Conflist: controller.Conflist{
			Path:     *conflistPath,
			Name:     "cnidaria",
			Bridge:   "cni0",
			IPAMName: *ipamStoreName,
			MTU:      *mtu,
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
