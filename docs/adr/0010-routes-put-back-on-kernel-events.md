# ADR 0010: Routes are put back when the kernel reports them gone

- Status: Accepted (2026-10-17)

## Context

ADR 0006 has a route removed behind the daemon's back put back by the next reconcile,
which runs on every Node event and on a timer. Nothing about losing a route changes a
Node object, so in practice the timer is what puts it back, and the timer runs every five
minutes. Until then the node and its peers cannot reach each other's pods over that
family.

Losing every route at once is an ordinary event, not an operator's mistake. A network
manager that owns the uplink and is configured to manage routes it did not create —
systemd-networkd does so by default (`ManageForeignRoutes=yes`) — deletes all of them
when it restarts. Restarts happen on package updates, on provisioning tools re-running,
and on unattended upgrades. An interface going down and up, or its address being
removed and added again, takes the routes with it too.

What the kernel tells a netlink listener about these differs by family, which was
checked in a network namespace:

- A route deleted explicitly, as a network manager or an operator does it, is reported
  for both families.
- IPv6 routes flushed because their interface went down are reported.
- IPv4 routes flushed because their interface went down, or because the address that
  made their gateway on-link was removed, are **not** reported. The route is simply gone.

So watching route deletions alone covers the network manager case but leaves IPv4
silent in the others.

## Decision

The daemon subscribes to the kernel's route, link and address notifications through
netlink, in the namespace it installs routes in, and asks the route reconciler to run
when one of these is reported:

- **one of its own routes was deleted** — a route carrying cnidaria's protocol marker
  (ADR 0006), of either family. Someone else's routes going away is not a reason;
- **a link is up and has carrier** — the moment the IPv4 routes a link took down with it
  can be installed again;
- **an address of global scope was added** — the moment the IPv4 routes whose gateway
  became unreachable with an address can be installed again. Link-local addresses do
  not count; every new pod interface gets one.

The subscription also asks for one run as soon as it is listening, for whatever
happened before. A notification that could not be decoded asks for a run too, since it
may have been one that counts.

These go into the same single work-queue key as Node events. A reconcile reads the
table as it is when it runs, so a burst of notifications needs no more than one run
pending; the rest are dropped. The reconcile is the same full recompute and apply as
before, so these triggers add no second way of changing routes. Its own changes do not
start a loop: removing a stale route of its own triggers exactly one more run, which
finds nothing to do, and adding or replacing routes, links and addresses are not
reasons.

The timed reconcile of ADR 0006 stays, at the same interval. It is now the bound for a
loss nothing reports — such as an IPv4 route flushed while the link stays up and no
address returns — rather than for every loss.

A route that goes in although the table did not hold it with that next hop is logged
with its node, so a route put back leaves a trace; a reconcile that finds everything in
place logs nothing about routes.

If a subscription stops — its socket fails, which is also what happens when it
overflows and notifications are lost — the watch ends with an error, and with it the
manager, so the daemon is restarted by its DaemonSet. A daemon that kept running
without the watch would look healthy while being back to the five-minute wait. A
restart does not touch the routes already in place.

### Alternatives

- **A shorter timer only.** Simple, but the wait is still the interval, and making it
  seconds means reapplying routes and the iptables chain every few seconds on every
  node for something that happens a few times a year.
- **Route deletions only.** Covers the network manager restart, the case seen in
  practice, but leaves IPv4 routes lost to a link flap waiting for the timer, which the
  link and address notifications close at little cost.
- **Every link and address notification.** Pod churn produces several per pod; the
  filters above keep only the ones after which a lost route can come back.
- **Telling the network manager to leave foreign routes alone.** Correct on a node that
  runs one, but it is node configuration the daemon cannot see or enforce, and the
  other ways of losing routes remain.

## Consequences

- After a network manager restart, routes are back within the time one reconcile takes
  instead of up to the resync interval.
- The daemon holds three more netlink sockets, and the route reconciler runs a few more
  times when pods come and go on the node, since their interfaces coming up count as
  links coming up. A reconcile that changes nothing costs a read of the cache, a route
  replace per peer and family, and the iptables chain check.
- The kernel watch needs nothing beyond what installing routes already needs
  (`CAP_NET_ADMIN` in the host network namespace).
- The netns testbed (ADR 0008) covers it: with the watch running on one node, deleting
  that node's routes, and taking its uplink down and up and putting its address back,
  each bring the routes and pod-to-pod reachability back within seconds.
