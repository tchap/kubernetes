# Disrupted Pods Should Be Removed from Endpoints

Reported in [issue #116965](https://github.com/kubernetes/kubernetes/issues/116965) and [issue #124648](https://github.com/kubernetes/kubernetes/issues/124648)  
Proof of concept in [PR #125774](https://github.com/kubernetes/kubernetes/pull/125774)  
Reviewed with mimowo@, aojea@, bobbypage@, yujuhong@  
Shared with sig-node, sig-network, and sig-apps

---

## Problem

Traffic flows from clients to pods through an eventually consistent chain:

```
Client --> Load Balancer / kube-proxy --> Service --> EndpointSlice --> Pod
```

The endpointslice controller manages which pod IPs appear in EndpointSlice
objects. Downstream consumers like kube-proxy read EndpointSlices and program
iptables/IPVS rules. When a pod is removed from an EndpointSlice, it takes
~1s for kube-proxy to react and up to ~60s for external load balancers or
DNS-based service discovery.

When a pod is **gracefully deleted** (e.g. `kubectl delete pod` or a
Deployment rolling update), this works correctly:

1. `deletionTimestamp` is set on the pod.
2. The endpointslice controller sees `deletionTimestamp` and marks the
   endpoint as `Terminating=true`, `Ready=false`.
3. The kubelet runs the pod's `preStop` hook, sends SIGTERM, and waits for
   the termination grace period.
4. During step 3, the load balancer has time to drain the pod.

But when a pod is **disrupted by the kubelet** -- graceful node shutdown,
eviction (disk/memory pressure), or preemption -- the pod is never deleted.
There is no `deletionTimestamp`. The endpointslice controller has **no signal**
that the pod is going away, so traffic keeps flowing to a pod whose containers
are being killed.

The result: connection refused errors, HTTP 502s, and other client-visible
failures that are entirely avoidable.

### Where this happens in practice

- **Graceful node shutdown**: A cloud VM is reclaimed (spot instance) or an
  admin reboots a node. The kubelet terminates pods but never deletes them
  because the node might return.
- **Eviction**: The kubelet evicts pods due to resource pressure (disk,
  memory, PID limits).
- **Preemption**: The kubelet preempts lower-priority pods to make room for
  a critical pod.

### Why we need a leading indicator

Because the endpoint chain is eventually consistent, pods must be removed
from endpoints **before** they stop -- not after. The preStop hook and
SIGTERM grace period exist precisely to provide this window for graceful
deletion. For kubelet-initiated disruptions, no equivalent window exists
today.

**Kubernetes Principle**: Pods that are known to be disrupted should be
eagerly removed from endpoints so that eventually consistent load balancers
have time to propagate the removal. Readiness is a trailing indicator;
for this problem we need a leading indicator.

**Kubernetes Principle**: Pods should behave consistently whether they are
gracefully terminated by the control plane (`deletionTimestamp` set),
gracefully disrupted by the kubelet (eviction), or forcibly disrupted by
the kubelet (hard threshold eviction).

---

## Cause

There is no signal sent by the kubelet during graceful node shutdown or
other sources of abnormal termination that results in the pod being removed
from endpoints before the containers stop.

The `DisruptionTarget` pod condition (GA since v1.31, KEP 3329) is set by
the kubelet during eviction, preemption, and graceful node shutdown. It is
the right signal, but two problems prevent it from being useful:

1. **It is delayed.** The kubelet's status manager suppresses
   `DisruptionTarget` from reaching the API server until the pod transitions
   to a terminal phase AND all containers have stopped.
2. **It is ignored.** The endpointslice and endpoints controllers do not
   consume `DisruptionTarget` at all.

### Why DisruptionTarget is delayed (verified)

`mergePodStatus()` in `pkg/kubelet/status/status_manager.go:1375-1390`
explicitly gates sending DisruptionTarget:

```go
if c.Type == v1.DisruptionTarget {
    if transitioningToTerminalPhase && !couldHaveRunningContainers {
        // only now send DisruptionTarget to the API server
    }
}
```

This was introduced by [PR #108366](https://github.com/kubernetes/kubernetes/pull/108366)
to prevent the scheduler from reclaiming resources before containers stop.
The terminal phase delay is correct and must be preserved. But coupling
DisruptionTarget to the same gate was unnecessary -- the condition and the
phase serve different consumers.

### The full timeline today

Inside `SyncTerminatingPod` (`pkg/kubelet/kubelet.go:2289`):

```
Line 2315:  SetPodStatus() -- caches DisruptionTarget + terminal phase
              ↓ mergePodStatus suppresses BOTH (containers still running)
Line 2323:  StopLivenessAndStartup()
Line 2326:  killPod() -- BLOCKS (preStop hooks, SIGTERM, grace period)
              ... containers running for seconds to minutes ...
Line 2336:  RemovePod() -- stops readiness probes
Line 2401:  SetPodStatus() again -- final status
              ↓ mergePodStatus NOW sends both (containers stopped)
```

By the time DisruptionTarget reaches the API server, the containers are dead.

However, the status manager runs a **separate goroutine** that syncs
cached status to the API asynchronously (`status_manager.go:283`). The
first `SetPodStatus()` at line 2315 happens **before** `killPod()` at
line 2326. If `mergePodStatus` stops suppressing DisruptionTarget, the
condition will reach the API server while containers are still alive --
during the preStop / SIGTERM grace period.

---

## Evaluated Signals

### Pod readiness (`PodReady` condition)

Readiness is a **trailing** indicator -- probes must fail before the pod is
marked unready, which takes time (probe interval x failure threshold). Pods
without readiness probes are always ready.

Overriding readiness during disruption would break workloads that
intentionally remain ready during graceful termination to finish in-flight
requests.

Two existing bugs further degrade readiness during disruption:

- [#105780](https://github.com/kubernetes/kubernetes/issues/105780): During
  graceful node shutdown, all probes are disabled. Pods remain "Ready"
  throughout.
- [#124648](https://github.com/kubernetes/kubernetes/issues/124648): During
  eviction, the internal phase is set to `Failed` before containers stop,
  causing the readiness probe worker to exit early. The pod never transitions
  to `NotReady`.

**Kubernetes Principle**: Pod readiness should remain orthogonal to
disruption because readiness is a user-defined signal consumed by components
beyond networking.

### Pod deletion (`deletionTimestamp`)

The endpointslice controller already uses `deletionTimestamp` as a
termination signal (`ShouldPodBeInEndpoints()` at
`staging/src/k8s.io/endpointslice/util/controller_utils.go:192`). This
works well for graceful deletion but cannot be used for kubelet-initiated
disruption:

- Graceful node shutdown cannot delete pods because the node might return.
- Deletion removes the pod's final status, which users and controllers need.
- Some workloads want to survive reboots with local state intact.

**Kubernetes Principle**: Pods should not be deleted by Kubernetes
controllers unless the user has opted in (via a workload controller, admin
action, or automated drain).

### DisruptionTarget condition

`DisruptionTarget` (GA since v1.31, gate removed in v1.34) signals that a
pod is being disrupted by infrastructure. Set by the kubelet during:

- Eviction (`pkg/kubelet/eviction/eviction_manager.go:432-438`)
- Graceful node shutdown (`pkg/kubelet/nodeshutdown/nodeshutdown_manager.go:160-166`)
- Preemption (`pkg/kubelet/preemption/preemption.go:109-115`)

Its scope is intentionally restricted to infrastructure-initiated
disruptions. Workload-caused exits (container exit, OOM) are excluded to
preserve the job controller's ability to safely retry pods.

Currently consumed only by: job controller (failure policy), taint eviction
controller, pod GC controller, scheduler preemption. Zero references in
any endpoints controller code.

**This is the right signal for Phase 1** -- it already exists, is already
set at the right time, and just needs to be unblocked and consumed.

### PendingTermination condition (proposed)

A new condition set by the kubelet as soon as the pod worker enters the
terminating state, for **any** reason. This would cover all scenarios that
DisruptionTarget cannot:

| Scenario | DisruptionTarget | PendingTermination |
|----------|:----------------:|:------------------:|
| Graceful node shutdown | Yes | Yes |
| Eviction (resource pressure) | Yes | Yes |
| Kubelet preemption | Yes | Yes |
| Scheduler preemption | Yes | Yes |
| Pod deleted (`kubectl delete`) | No (has deletionTimestamp) | Yes |
| Container exits (RestartNever) | No | Yes |
| ActiveDeadlineSeconds exceeded | No | Yes |
| Container OOM-killed (RestartOnFailure) | No | Yes |

**This requires a new API and is deferred to Phase 2.**

---

## Proposed Implementation

### Phase 1: Eagerly propagate DisruptionTarget to endpoints

No new API types. Leverages an existing GA condition. Two changes:

#### 1. Kubelet: send DisruptionTarget before containers stop

Modify `mergePodStatus()` in `pkg/kubelet/status/status_manager.go` to
decouple DisruptionTarget from the terminal phase gate. When the feature
gate is enabled, send DisruptionTarget immediately. When disabled, preserve
the existing behavior:

```go
if c.Type == v1.DisruptionTarget {
    if utilfeature.DefaultFeatureGate.Enabled(features.DisruptionTargetSignalsEndpointTerminating) ||
        (transitioningToTerminalPhase && !couldHaveRunningContainers) {
        updateLastTransitionTime(&newPodStatus, &oldPodStatus, c.Type)
        if _, c := podutil.GetPodConditionFromList(newPodStatus.Conditions, c.Type); c != nil {
            podConditions = statusutil.ReplaceOrAppendPodCondition(podConditions, c)
        }
    }
}
```

The terminal phase delay (lines 1411-1417) remains unchanged. The scheduler
resource-reclaim invariant from PR #108366 is preserved.

**Why this is timely enough**: `SetPodStatus()` is called at line 2315,
before `killPod()` at line 2326. The status manager's sync goroutine runs
concurrently and will PATCH the API server while containers are still alive.
`mergePodStatus` is the **only gate** preventing early propagation.

#### 2. Endpoints controllers: consume DisruptionTarget

**Endpointslice controller** (staging library):

- `podToEndpoint()` (`staging/src/k8s.io/endpointslice/utils.go:40`):
  Set `terminating = true` when DisruptionTarget is present (in addition
  to `DeletionTimestamp`). This cascades to `Ready=false` automatically.
- `podEndpointsChanged()` (`staging/.../util/controller_utils.go:208`):
  Detect DisruptionTarget condition changes to trigger reconciliation.
  This check is unconditional (cheap, harmless when gate is off).
- Wire the feature gate via `ReconcilerOption` (matching the existing
  `WithPreferSameTrafficDistributionEnabled` pattern).

**Legacy endpoints controller** (`pkg/controller/endpoint/endpoints_controller.go`):

- `addEndpointSubset()` (line 640): When the gate is enabled and
  DisruptionTarget is present, treat the pod as not-ready.

#### Feature gate

`DisruptionTargetSignalsEndpointTerminating`, Alpha, default off.

Starting at Alpha (default off) gives the ecosystem one release cycle to
adapt, since controllers and operators may currently assume DisruptionTarget
only appears on pods in a terminal phase.

#### Timeline after Phase 1

```
T+0s   Kubelet receives shutdown signal
T+0s   Kubelet sets DisruptionTarget=True in status cache
T+0s   Status manager sends DisruptionTarget to API server    <-- NEW
T+~1s  Endpointslice controller marks endpoint Terminating    <-- NEW
T+~2s  kube-proxy updates iptables, traffic stops flowing
T+Ns   preStop hook runs, SIGTERM sent, containers stop
```

#### What Phase 1 does NOT cover

- Pods that terminate because their main process exits (RestartNever Jobs).
- Pods that exceed their `activeDeadlineSeconds`.
- Any termination not initiated by infrastructure disruption.

#### Job controller safety (verified)

The job controller uses DisruptionTarget for pod failure policy retry
decisions. Sending DisruptionTarget early could theoretically cause
premature retries. However, the job controller
(`pkg/controller/job/pod_failure_policy.go`) only evaluates conditions on
pods that have reached a terminal phase. Since we continue to delay the
terminal phase transition, the job controller will not see DisruptionTarget
on a Running pod. No behavior change for jobs.

### Phase 2: PendingTermination condition (future KEP)

A new condition covering all termination scenarios, including those
DisruptionTarget cannot address.

**Definition**: Set to `True` when the kubelet pod worker enters the
terminating state (`startedTerminating` flag is true), regardless of the
reason. This is the earliest unambiguous point in the kubelet lifecycle.

**Lifecycle**:

- Set to `True` when the pod worker begins termination.
- Removed if the kubelet restarts and the source of termination was not
  durable (was not a `deletionTimestamp`). This handles transient
  disruptions like reboots.
- Set to `False` when all pod resources (volumes, network) are fully
  released (complementing KEP 4577).

**Endpoints consumption**: If any of `deletionTimestamp`, `DisruptionTarget`,
or `PendingTermination` indicates the pod is going away, treat it as
terminating.

**API requirements**: New `PodConditionType`, new feature gate
(`PodPendingTerminationCondition`), standard Alpha -> Beta -> GA graduation.
Separate KEP required.

**Open design questions for Phase 2**:

- For RestartOnFailure pods where a container fails: is PendingTermination
  set even though the container will be restarted? (Recommended: no --
  only set when the pod worker is terminating the pod, not individual
  container restarts.)
- For RestartAlways pods during graceful shutdown: PendingTermination must
  be reliably removed after reboot. Since the kubelet has no local store,
  the condition must be reconstructed from API state on startup.

---

## Alternatives Considered

### Delete pods during graceful node shutdown

- Deleting a pod removes its final status (exit codes, OOM events), which
  operators depend on.
- The problem affects eviction and preemption too, not just shutdown.
- Nodes can come back after reboot -- deletion forces unnecessary
  rescheduling.
- Some workloads want to survive reboots with local state intact.
- Kubernetes cannot safely determine whether a shutdown is permanent.

### Override pod readiness for terminating pods

- Suppresses the correct value from readiness probes, breaking
  orthogonality of readiness and termination (part of the EndpointSlice
  public API).
- Would block workloads that use readiness during graceful deletion
  (preStop hooks that drain connections while remaining "ready").

### Expand DisruptionTarget to include all disruption types

- Backward-incompatible change to job controller retry semantics.
- Breaks the intent of DisruptionTarget: "the workload itself did not
  cause this."

### Add a "dual" condition alongside DisruptionTarget

- Not needed by the DisruptionTarget consumer (the job controller).
- Having PendingTermination orthogonal avoids coupling and overlap
  management.
- PendingTermination covers a broader scope (resource release tracking).

### Do nothing

- Limits graceful node shutdown to workloads that tolerate ungraceful
  disruption.
- Makes behavior distribution-dependent.

---

## Related Work

### Readiness probe bugs (parallel fixes)

These bugs compound the endpoints problem and should be fixed
independently:

- [#105780](https://github.com/kubernetes/kubernetes/issues/105780):
  All probes disabled during graceful node shutdown. Pods remain "Ready"
  throughout. Fix: continue readiness probes during shutdown, or
  explicitly mark pods not-ready.
- [#124648](https://github.com/kubernetes/kubernetes/issues/124648):
  During eviction, internal phase set to `Failed` before containers stop
  causes readiness probe worker to exit early. Pod never transitions to
  `NotReady`. Fix: probe worker should check container state, not phase.

Fixing these would provide partial relief even without Phase 1, but they
are trailing indicators -- Phase 1's leading indicator is still needed.

### Graceful node shutdown and restart policies

Not all graceful node shutdowns result in node removal. The current
implementation is geared toward nodes that will not return (spot instances)
or nodes where workloads have been drained. However, graceful shutdown
should work for all distributions and use cases.

For RestartAlways pods, the kubelet should arguably not set terminal phases
during graceful shutdown if the node will return. This would require:

- Avoiding terminal phases for RestartAlways pods during shutdown (perhaps
  setting phase to `Pending` instead).
- After reboot, the kubelet restarts pods normally.
- This is a behavior change requiring an opt-in kubelet flag.
- RestartNever pods should still get terminal phases if the status update
  propagates before shutdown completes.

### KEP 4563: EvictionRequest API

[KEP 4563](https://github.com/kubernetes/enhancements/issues/4563) (Alpha
targeting v1.37) introduces a declarative `EvictionRequest` API for
cooperative pod eviction. An `EvictionRequest` object lets multiple
requesters (node drain, cluster autoscaler, maintenance controllers) signal
that a pod should be evicted, and lets interceptors (the application itself,
an operator, or a sidecar) perform graceful preparation before the pod is
actually terminated -- data migration, leader election handoff, connection
draining, etc.

**Relationship to this proposal:**

KEP 4563 and this proposal are **complementary, not competing**. They
address different layers of the same problem:

- **KEP 4563 solves orchestration**: who decides when a pod should be
  evicted, and what application-specific preparation happens before
  termination begins. It provides the *coordination protocol* above the
  kubelet.
- **This proposal solves the infrastructure signal**: once termination
  begins (whether triggered by an EvictionRequest, kubelet eviction, node
  shutdown, or preemption), the pod must be removed from endpoints before
  containers stop. It provides the *network-level safety net* below the
  application.

KEP 4563 does not address endpoint removal. Its interceptors *could*
perform traffic draining as part of their logic, but this is left entirely
to each interceptor's discretion. There is no standardized contract
ensuring that endpoints are updated before containers are killed. Even with
EvictionRequest, the following scenarios are unaddressed:

- Interceptors that do not implement traffic draining.
- Pods without any interceptors configured.
- Hard evictions (kubelet resource pressure) that bypass the
  EvictionRequest flow entirely.
- Graceful node shutdown, which terminates pods directly without creating
  EvictionRequests.

This proposal fills that gap by ensuring that **any** kubelet-initiated
disruption -- whether it went through an EvictionRequest or not -- results
in an early endpoint removal signal via DisruptionTarget. An interceptor
that wants to do application-level draining can still do so; this proposal
ensures the infrastructure-level draining happens regardless.

**Integration opportunity**: When an EvictionRequest triggers pod
termination through the kubelet's eviction path, DisruptionTarget is set by
the eviction manager (`eviction_manager.go:432-438`). With this proposal's
Phase 1 change, that condition will immediately propagate to the API server
and cause endpoint removal -- giving EvictionRequest interceptors and the
load balancer chain time to react in parallel.

### KEP 4577: Pod resource release tracking

A complementary proposal addressing detection of when all pod resources
(volumes, network) are fully released. PendingTermination (Phase 2) could
serve as the bookend signal: `True` when termination begins, `False` when
resources are released.

---

## References

| Reference | Status | Role |
|-----------|--------|------|
| [#116965](https://github.com/kubernetes/kubernetes/issues/116965) | Open | Primary motivating issue |
| [#124648](https://github.com/kubernetes/kubernetes/issues/124648) | Open | Readiness probe bug during eviction |
| [#105780](https://github.com/kubernetes/kubernetes/issues/105780) | Open | Probes disabled during shutdown |
| [PR #125774](https://github.com/kubernetes/kubernetes/pull/125774) | Closed (stale) | Proof of concept (validates approach) |
| [PR #108366](https://github.com/kubernetes/kubernetes/pull/108366) | Merged | Root cause of the delay (must preserve phase invariant) |
| [KEP 3329](https://github.com/kubernetes/enhancements/tree/master/keps/sig-apps/3329-retriable-and-non-retriable-failures) | GA (v1.31) | Introduced DisruptionTarget |
| [KEP 2000](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/2000-graceful-node-shutdown) | GA | Graceful node shutdown |
| [KEP 4563](https://github.com/kubernetes/enhancements/issues/4563) | Alpha (v1.37) | EvictionRequest API (complementary) |
| [KEP 4577](https://github.com/kubernetes/enhancements/pull/4577) | Proposed | Pod resource release tracking |

### Code locations (verified against kubernetes/kubernetes @ 127f3c0e117)

| Component | File | Key Lines |
|-----------|------|-----------|
| DisruptionTarget delay | `pkg/kubelet/status/status_manager.go` | 1375-1390 |
| Terminal phase delay | `pkg/kubelet/status/status_manager.go` | 1411-1417 |
| SyncTerminatingPod | `pkg/kubelet/kubelet.go` | 2289-2408 |
| EndpointSlice terminating | `staging/src/k8s.io/endpointslice/utils.go` | 40 |
| ShouldPodBeInEndpoints | `staging/.../util/controller_utils.go` | 180-197 |
| Endpoint change detection | `staging/.../util/controller_utils.go` | 208-244 |
| Node shutdown sets condition | `pkg/kubelet/nodeshutdown/nodeshutdown_manager.go` | 160-166 |
| Eviction sets condition | `pkg/kubelet/eviction/eviction_manager.go` | 432-438 |
| Legacy endpoints ready check | `pkg/controller/endpoint/endpoints_controller.go` | 640 |
