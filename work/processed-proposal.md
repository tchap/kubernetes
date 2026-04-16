# Processed Proposal: Remove Disrupted Pods from Endpoints Early

Original: `work/original-proposal.md`  
Processed: 2026-04-16

---

## Background: How Traffic Reaches a Pod

To understand this proposal you need a mental model of how traffic flows from a
client to a pod in Kubernetes. The chain looks like this:

```
Client --> Load Balancer / kube-proxy --> Service --> EndpointSlice --> Pod
```

A **Service** is a stable network identity. It doesn't serve traffic itself --
it points to a set of **endpoints**, which are the IP addresses of the pods
that back it. These endpoints live in **EndpointSlice** objects, managed by the
endpointslice controller in the control plane.

When a pod starts and becomes ready, the endpointslice controller adds it to
the EndpointSlice. When a pod is deleted or becomes unready, the controller
removes it. Downstream consumers like kube-proxy read EndpointSlices and
program iptables/IPVS rules so that traffic to the Service IP gets forwarded to
one of the listed pod IPs.

**This whole chain is eventually consistent.** When the endpointslice controller
removes a pod, it takes time for kube-proxy to pick that up (~1s), and even
longer for external load balancers or DNS-based service discovery (~60s). This
latency is the reason the problem exists.

---

## The Problem

When a pod is **gracefully deleted** (e.g. `kubectl delete pod` or a Deployment
rolling update), everything works correctly:

1. `deletionTimestamp` is set on the pod.
2. The endpointslice controller sees `deletionTimestamp` and removes the pod
   from EndpointSlices immediately.
3. The kubelet runs the pod's `preStop` hook, sends SIGTERM, and waits for
   the termination grace period.
4. During step 3, the load balancer has time to drain the pod from its
   rotation.

But when a pod is **disrupted by the kubelet** -- graceful node shutdown,
eviction (e.g. disk pressure), or preemption by a higher-priority pod -- the
pod is never deleted. There is no `deletionTimestamp`. The endpointslice
controller has **no signal** that the pod is going away, so it stays in
EndpointSlices. Traffic keeps flowing to a pod whose containers are being
killed.

The result: connection refused errors, HTTP 502s, or worse -- clients see
failures that are entirely avoidable.

### Where this happens in practice

- **Graceful node shutdown**: A cloud VM is being reclaimed (spot instance) or
  an admin reboots a node. The kubelet receives a shutdown signal and begins
  terminating pods. Pods are never deleted because the node might come back.
- **Eviction**: The kubelet evicts pods due to resource pressure (disk, memory,
  PID limits). Pods are terminated but not deleted.
- **Preemption**: The kubelet preempts lower-priority pods to make room for a
  critical pod.

---

## Why Can't We Just Use Existing Signals?

The proposal evaluates four signals that could tell the endpoints controller
that a pod is going away. Understanding why each one falls short is key.

### Signal 1: Pod Readiness (`PodReady` condition)

**What it is:** A pod is "ready" when its readiness probe passes (or if it has
no readiness probe, it's always ready). The endpointslice controller uses
readiness to decide whether to include a pod.

**Why it doesn't work:**

- Readiness is a **trailing** indicator. Probes have to fail first, which
  takes time (probe interval + failure threshold). We need a **leading**
  indicator -- something that says "this pod *will* go away" before it
  actually does.
- Pods without readiness probes are always ready. Many workloads don't define
  them.
- Overriding readiness during disruption would break workloads that
  intentionally remain ready during graceful termination (e.g. to finish
  in-flight requests).

### Signal 2: Pod Deletion (`deletionTimestamp`)

**What it is:** When someone calls DELETE on a pod, `deletionTimestamp` is set.
The endpointslice controller already uses this as a signal to remove the pod.

**Why it doesn't work for disruption:**

- Graceful node shutdown, eviction, and preemption do NOT delete the pod.
  They terminate it locally on the kubelet without going through the API
  server's DELETE path.
- We can't just delete pods during shutdown because:
  - The node might come back (reboot vs. permanent shutdown).
  - Deletion removes the pod's final status, which users and controllers
    may need.
  - Some workloads want to survive reboots with local state intact.

### Signal 3: `DisruptionTarget` Condition

**What it is:** A pod condition (like `Ready` or `Initialized`) introduced by
KEP 3329 (Pod Failure Policy, GA in v1.31). It signals that a pod is being
disrupted by infrastructure, not by its own workload. The kubelet sets it
during eviction, preemption, and graceful node shutdown.

**Why it *almost* works but doesn't today:**

- It exists and is already set in the right places.
- **But it is delayed.** The kubelet's status manager intentionally holds
  back DisruptionTarget from the API server until the pod reaches a terminal
  phase (Failed/Succeeded) AND all containers have stopped. This was done to
  avoid a scheduler race condition (PR #108366).
- By the time DisruptionTarget reaches the API server, the containers are
  already dead. The endpoints controller couldn't act on it even if it
  wanted to.
- Additionally, the endpoints controller doesn't look at DisruptionTarget
  at all today.
- Finally, DisruptionTarget only covers *infrastructure-initiated*
  disruptions. Pods that terminate because their main container exits (e.g.
  a Job pod finishing) are not covered, because DisruptionTarget's contract
  is specifically "the workload itself did not cause this."

### Signal 4: `PendingTermination` Condition (proposed, does not exist yet)

**What it would be:** A new condition that the kubelet sets as soon as it
begins terminating a pod, for *any* reason -- disruption, deletion, container
exit, active deadline exceeded, etc. It would be the universal "this pod is
being terminated" signal.

**Why it's needed:** DisruptionTarget has a restricted scope by design (only
infrastructure disruptions, not workload-caused exits). PendingTermination
would cover all the remaining cases.

---

## The Proposed Solution (Two Phases)

### Phase 1: Send DisruptionTarget Early, Consume It in Endpoints

This phase addresses the most impactful scenarios (graceful node shutdown,
eviction, preemption) without any new API types.

**Step 1 -- Kubelet change: stop delaying DisruptionTarget.**

Today, `mergePodStatus()` in the kubelet's status manager suppresses
DisruptionTarget until the pod is terminal and containers have stopped:

```go
// status_manager.go:1375-1390
if transitioningToTerminalPhase && !couldHaveRunningContainers {
    // only NOW send DisruptionTarget to the API server
}
```

The fix: decouple DisruptionTarget from the terminal phase gate. Send
DisruptionTarget to the API server as soon as the kubelet sets it, while
continuing to delay the terminal phase transition (to preserve the scheduler
resource-reclaim invariant).

**Step 2 -- Endpointslice controller change: consume DisruptionTarget.**

Update the endpointslice controller so that a pod with
`DisruptionTarget=True` is treated the same as a pod with
`deletionTimestamp` set:

- `ShouldPodBeInEndpoints()`: exclude pods with DisruptionTarget (unless
  `includeTerminating` is true).
- `podToEndpoint()`: set `terminating = true` when DisruptionTarget is
  present.
- `podEndpointsChanged()`: detect DisruptionTarget condition changes so
  the controller reconciles.

**Step 3 -- Feature gate.**

Gate the new behavior behind a feature gate (the POC PR used
`PodDisruptionConditionSignalsTerminating`), defaulted to on. This allows
rollback if issues are discovered.

**What this achieves:**

After Phase 1, the timeline for a pod during graceful node shutdown looks
like:

```
T+0s   Kubelet receives shutdown signal
T+0s   Kubelet sets DisruptionTarget=True on the pod
T+0s   Status manager sends DisruptionTarget to API server    <-- NEW
T+~1s  Endpointslice controller removes pod from endpoints    <-- NEW
T+~2s  kube-proxy updates iptables, traffic stops flowing
T+Ns   preStop hook runs, SIGTERM sent, containers stop
```

Compare with today:

```
T+0s   Kubelet receives shutdown signal
T+Ns   Containers stop
T+Ns   Status manager sends DisruptionTarget + terminal phase  <-- too late
T+N+1s Endpointslice controller could react, but pod is dead
```

**What it does NOT cover:**

- Pods that terminate because their main process exits (RestartNever Jobs).
- Pods that exceed their `activeDeadlineSeconds`.
- Any termination not initiated by infrastructure disruption.

### Phase 2: Add PendingTermination Condition

This phase introduces a new pod condition to cover all remaining cases.

**PendingTermination** would be set by the kubelet at the moment the pod
worker enters the terminating state -- regardless of why. This is the
earliest possible point in the kubelet's pod lifecycle where termination
is decided.

| Scenario | DisruptionTarget | PendingTermination |
|----------|:----------------:|:------------------:|
| Graceful node shutdown | Yes | Yes |
| Eviction (disk/memory pressure) | Yes | Yes |
| Kubelet preemption | Yes | Yes |
| Scheduler preemption | Yes | Yes |
| Pod deleted (`kubectl delete`) | No (has deletionTimestamp) | Yes |
| Container exits (RestartNever) | No | Yes |
| ActiveDeadlineSeconds exceeded | No | Yes |
| Container OOM-killed (RestartOnFailure) | No | Yes |

The endpoints controller would check: if **any** of `deletionTimestamp`,
`DisruptionTarget`, or `PendingTermination` indicates the pod is going
away, treat it as terminating.

**Lifecycle of the condition:**

- Set to `True` when the kubelet pod worker begins terminating the pod.
- Removed if the kubelet restarts and the source of termination was not
  durable (i.e., not a `deletionTimestamp`). This handles the case where a
  node reboots and the disruption is no longer relevant.
- Set to `False` when all pod resources (volumes, network) are fully
  released (complementing KEP 4577 for resource cleanup tracking).

**This requires a new KEP** with its own feature gate
(`PodPendingTerminationCondition`) and standard Alpha -> Beta -> GA
graduation.

---

## Why Not Just Delete Pods During Shutdown?

This is the most intuitive alternative and worth addressing directly:

1. **Information loss.** Deleting a pod removes its final status. Operators
   and monitoring tools that inspect pod exit codes, OOM events, or failure
   reasons lose that data.
2. **Not just shutdown.** The problem affects eviction and preemption too.
   Deleting pods on eviction would be even more surprising.
3. **Nodes can come back.** A reboot is not the same as a node going away
   forever. Deleting pods would force re-scheduling even when the node
   returns in seconds.
4. **Workloads with local state.** Some pods (caches, local databases) want
   to survive a reboot and resume on the same node.

---

## Open Issues Noted in the Proposal

Two existing bugs make the situation worse and are worth fixing in parallel:

1. **Issue #105780**: During graceful node shutdown, all probes are disabled.
   Pods remain "Ready" throughout shutdown, so even the trailing readiness
   signal is suppressed.
2. **Issue #124648**: During eviction, the internal pod phase is set to
   `Failed` before containers stop. The readiness probe worker sees the
   terminal phase and exits. The pod never transitions to `NotReady`.

Both of these are independent bugs, but fixing them would provide partial
relief even without Phase 1.

---

## Glossary

| Term | Meaning |
|------|---------|
| **Terminating** | Pod has `deletionTimestamp` set. The kubelet is gracefully shutting it down. |
| **Terminal** | Pod is in phase `Succeeded` or `Failed`. Containers are done. |
| **Disruption** | Infrastructure-initiated event that causes pod termination (shutdown, eviction, preemption). |
| **DisruptionTarget** | Pod condition indicating the pod is being disrupted by infrastructure. GA since v1.31. |
| **EndpointSlice** | API object listing the network endpoints (pod IPs + ports) backing a Service. |
| **Readiness probe** | User-defined health check. When it fails, the pod is removed from endpoints. |
| **preStop hook** | User-defined command that runs before SIGTERM is sent during graceful termination. Provides a window for traffic draining. |
| **Eventually consistent** | Changes propagate through the system with latency, not instantaneously. |
