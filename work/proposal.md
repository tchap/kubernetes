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

### Why DisruptionTarget is delayed

The kubelet categorizes pod conditions into three ownership classes
(`pkg/kubelet/types/pod_status.go`):

- **Owned by kubelet** (`PodConditionByKubelet`, line 36): `PodScheduled`,
  `PodReady`, `ContainersReady`, etc. -- managed directly in pod status.
- **Shared** (`PodConditionSharedByKubelet`, line 56): only
  `DisruptionTarget`. This is the special case -- the kubelet writes it, but
  other controllers (job controller, taint eviction, scheduler) also write
  it for their own reasons.

```go
// pkg/kubelet/types/pod_status.go:55-58
func PodConditionSharedByKubelet(conditionType v1.PodConditionType) bool {
    return conditionType == v1.DisruptionTarget
}
```

Because DisruptionTarget is "shared" rather than "owned", it gets special
handling in `mergePodStatus()` (`pkg/kubelet/status/status_manager.go:1359`).
This function is called on every status update to decide which conditions
actually get sent to the API server. The relevant section:

```go
// pkg/kubelet/status/status_manager.go:1370-1392
for _, c := range newPodStatus.Conditions {
    if kubetypes.PodConditionByKubelet(c.Type) {
        podConditions = append(podConditions, c)       // always sent
    } else if kubetypes.PodConditionSharedByKubelet(c.Type) {
        if c.Type == v1.DisruptionTarget {
            // guard the update of the DisruptionTarget condition with a
            // check to ensure it will only be sent once all containers
            // have terminated and the phase is terminal.
            if transitioningToTerminalPhase && !couldHaveRunningContainers {
                updateLastTransitionTime(...)
                if _, c := podutil.GetPodConditionFromList(...); c != nil {
                    podConditions = statusutil.ReplaceOrAppendPodCondition(podConditions, c)
                }
            }
        }
    }
}
```

The guard `transitioningToTerminalPhase && !couldHaveRunningContainers`
means DisruptionTarget only reaches the API server after the pod has moved
from Running to Failed/Succeeded AND all containers have exited. This was
introduced by [PR #108366](https://github.com/kubernetes/kubernetes/pull/108366)
to prevent the scheduler from reclaiming resources before containers stop.
The terminal phase delay is correct and must be preserved. But coupling
DisruptionTarget to the same gate was unnecessary -- the condition and the
phase serve different consumers.

### How the kubelet sets DisruptionTarget

All three disruption sources set DisruptionTarget with reason
`TerminationByKubelet` via a `podStatusFn` callback passed to `killPodFunc`:

**Graceful node shutdown** (`pkg/kubelet/nodeshutdown/nodeshutdown_manager.go:153-166`):

```go
if err := m.killPodFunc(pod, false, &gracePeriodOverride, func(status *v1.PodStatus) {
    if status.Phase != v1.PodSucceeded {
        status.Phase = v1.PodFailed
    }
    status.Message = nodeShutdownMessage
    status.Reason = nodeShutdownReason
    podutil.UpdatePodCondition(status, &v1.PodCondition{
        Type:    v1.DisruptionTarget,
        Status:  v1.ConditionTrue,
        Reason:  v1.PodReasonTerminationByKubelet,
        Message: nodeShutdownMessage,
    })
}); err != nil {
```

**Eviction** (`pkg/kubelet/eviction/eviction_manager.go:432-438`):

```go
condition := &v1.PodCondition{
    Type:    v1.DisruptionTarget,
    Status:  v1.ConditionTrue,
    Reason:  v1.PodReasonTerminationByKubelet,
    Message: message,
}
if m.evictPod(logger, pod, gracePeriodOverride, message, annotations, condition) {
```

**Kubelet preemption** (`pkg/kubelet/preemption/preemption.go:105-116`):

```go
err := c.killPodFunc(pod, true, nil, func(status *v1.PodStatus) {
    status.Phase = v1.PodFailed
    status.Reason = events.PreemptContainer
    status.Message = message
    podutil.UpdatePodCondition(status, &v1.PodCondition{
        Type:    v1.DisruptionTarget,
        Status:  v1.ConditionTrue,
        Reason:  v1.PodReasonTerminationByKubelet,
        Message: "Pod was preempted by Kubelet to accommodate a critical pod.",
    })
})
```

In all cases, the `podStatusFn` callback sets both the terminal phase and
DisruptionTarget on the status object. This callback is applied inside
`SyncTerminatingPod` before `killPod()` is called.

### The full termination timeline

Inside `SyncTerminatingPod` (`pkg/kubelet/kubelet.go:2289`):

```go
// Line 2311-2315: Generate status, apply podStatusFn, cache it
apiPodStatus := kl.generateAPIPodStatus(ctx, pod, podStatus, false)
if podStatusFn != nil {
    podStatusFn(&apiPodStatus)  // sets DisruptionTarget + Failed phase
}
kl.statusManager.SetPodStatus(logger, pod, apiPodStatus)
//   ^-- caches status, sends to podStatusChannel
//       BUT mergePodStatus suppresses DisruptionTarget AND the phase
//       because couldHaveRunningContainers is still true

// Line 2323: Stop liveness and startup probes
kl.probeManager.StopLivenessAndStartup(pod)

// Line 2325-2326: Kill containers (BLOCKS for grace period)
p := kubecontainer.ConvertPodStatusToRunningPod(kl.getRuntime().Type(), podStatus)
if err := kl.killPod(ctx, pod, p, gracePeriod); err != nil {
    // ...
}

// Line 2336: Remove all probes (including readiness) AFTER containers stop
kl.probeManager.RemovePod(pod)

// Line 2401-2402: Final status update -- containers now stopped
apiPodStatus = kl.generateAPIPodStatus(ctx, pod, stoppedPodStatus, true)
kl.statusManager.SetPodStatus(logger, pod, apiPodStatus)
//   ^-- NOW mergePodStatus sends both DisruptionTarget and terminal phase
//       because couldHaveRunningContainers is false
```

By the time DisruptionTarget reaches the API server, the containers are dead.

### Why modifying `mergePodStatus` is sufficient

The status manager's `SetPodStatus()` (`status_manager.go:464`) caches the
status and signals its background goroutine via a buffered channel:

```go
// status_manager.go:993-999 (inside updateStatusInternal)
m.podStatuses[pod.UID] = newStatus

select {
case m.podStatusChannel <- struct{}{}:
default:
    // there's already a status update pending
}
```

The background goroutine (`status_manager.go:283`) picks up the signal and
patches the API server:

```go
// status_manager.go:283-294
go wait.Forever(func() {
    for {
        select {
        case <-m.podStatusChannel:
            logger.V(4).Info("Syncing updated statuses")
            m.syncBatch(ctx, false)
        case <-syncTicker:
            logger.V(4).Info("Syncing all statuses")
            m.syncBatch(ctx, true)
        }
    }
}, 0)
```

The first `SetPodStatus()` at line 2315 happens **before** `killPod()` at
line 2326. The status sync goroutine runs concurrently with the pod worker
that executes `killPod()`. If `mergePodStatus` stops suppressing
DisruptionTarget, the condition will reach the API server while containers
are still alive -- during the preStop / SIGTERM grace period.

The terminal phase delay (lines 1411-1417) is separate and remains intact:

```go
// pkg/kubelet/status/status_manager.go:1411-1417
if transitioningToTerminalPhase {
    if couldHaveRunningContainers {
        newPodStatus.Phase = oldPodStatus.Phase     // keep Running
        newPodStatus.Reason = oldPodStatus.Reason
        newPodStatus.Message = oldPodStatus.Message
    }
}
```

This preserves the scheduler resource-reclaim invariant from PR #108366.

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
termination signal. This is visible in two places:

**`ShouldPodBeInEndpoints()`** (`staging/src/k8s.io/endpointslice/util/controller_utils.go:180-197`)
decides whether a pod should appear in any EndpointSlice:

```go
func ShouldPodBeInEndpoints(pod *v1.Pod, includeTerminating bool) bool {
    if isPodTerminal(pod) {           // Succeeded or Failed -> exclude
        return false
    }
    if len(pod.Status.PodIP) == 0 && len(pod.Status.PodIPs) == 0 {
        return false                  // no IP -> exclude
    }
    if !includeTerminating && pod.DeletionTimestamp != nil {
        return false                  // terminating + not wanted -> exclude
    }
    return true
}
```

**`podToEndpoint()`** (`staging/src/k8s.io/endpointslice/utils.go:38-72`)
translates a pod into an EndpointSlice entry. The `terminating` field drives
the `ready` field:

```go
func podToEndpoint(pod *v1.Pod, node *v1.Node, service *v1.Service,
    addressType discovery.AddressType) discovery.Endpoint {

    serving := endpointutil.IsPodReady(pod)
    terminating := pod.DeletionTimestamp != nil          // <-- only signal
    ready := service.Spec.PublishNotReadyAddresses || (serving && !terminating)

    ep := discovery.Endpoint{
        Conditions: discovery.EndpointConditions{
            Ready:       &ready,
            Serving:     &serving,
            Terminating: &terminating,
        },
        // ...
    }
    return ep
}
```

This works well for graceful deletion but cannot be used for kubelet-initiated
disruption:

- Graceful node shutdown cannot delete pods because the node might return.
- Deletion removes the pod's final status, which users and controllers need.
- Some workloads want to survive reboots with local state intact.

**Kubernetes Principle**: Pods should not be deleted by Kubernetes
controllers unless the user has opted in (via a workload controller, admin
action, or automated drain).

### DisruptionTarget condition

`DisruptionTarget` (GA since v1.31, gate removed in v1.34) signals that a
pod is being disrupted by infrastructure. It is set in all three kubelet
disruption paths (shown above in "How the kubelet sets DisruptionTarget").

Its scope is intentionally restricted to infrastructure-initiated
disruptions. Workload-caused exits (container exit, OOM) are excluded to
preserve the job controller's ability to safely retry pods.

**Endpoint change detection** does not see DisruptionTarget today.
`podEndpointsChanged()` (`staging/src/k8s.io/endpointslice/util/controller_utils.go:208-244`)
determines whether a pod update should trigger endpoint reconciliation. It
checks three things and none of them detect DisruptionTarget:

```go
func podEndpointsChanged(oldPod, newPod *v1.Pod) (bool, bool) {
    labelsChanged := false
    if !reflect.DeepEqual(newPod.Labels, oldPod.Labels) ||
        !hostNameAndDomainAreEqual(newPod, oldPod) {
        labelsChanged = true
    }

    // 1. DeletionTimestamp changed
    if newPod.DeletionTimestamp != oldPod.DeletionTimestamp {
        return true, labelsChanged
    }
    // 2. Readiness changed
    if IsPodReady(oldPod) != IsPodReady(newPod) {
        return true, labelsChanged
    }
    // 3. Pod IPs changed
    if len(oldPod.Status.PodIPs) != len(newPod.Status.PodIPs) {
        return true, labelsChanged
    }
    for i := range oldPod.Status.PodIPs {
        if oldPod.Status.PodIPs[i].IP != newPod.Status.PodIPs[i].IP {
            return true, labelsChanged
        }
    }
    return false, labelsChanged
}
```

If DisruptionTarget appears on a pod and nothing else changes, the
endpointslice controller will not reconcile. This must be fixed for Phase 1.

Currently DisruptionTarget is referenced by six controllers. Zero references
exist in any endpoints controller code. The following analysis verifies that
sending DisruptionTarget early (on a Running pod, before containers stop) is
safe for every existing consumer.

#### Consumer 1: Job controller -- pod failure policy

`matchPodFailurePolicy()` (`pkg/controller/job/pod_failure_policy.go:36`)
matches pod conditions (including DisruptionTarget) against user-defined
failure policy rules:

```go
func matchPodFailurePolicy(podFailurePolicy *batch.PodFailurePolicy,
    failedPod *v1.Pod) (*string, bool, *batch.PodFailurePolicyAction) {
    // ...
    for index, podFailurePolicyRule := range podFailurePolicy.Rules {
        // matches on exit codes OR on pod conditions like DisruptionTarget
    }
}
```

**Why it is safe**: The function is only called on pods that have already
been classified as failed. All call sites guard with `isPodFailed(pod, job)`
(`job_controller.go:2169`), which requires `pod.Status.Phase == PodFailed`
or `pod.DeletionTimestamp != nil`:

```go
// pkg/controller/job/job_controller.go:2169-2179
func isPodFailed(p *v1.Pod, job *batch.Job) bool {
    if p.Status.Phase == v1.PodFailed {
        return true
    }
    if onlyReplaceFailedPods(job) {
        return false
    }
    return p.DeletionTimestamp != nil && p.Status.Phase != v1.PodSucceeded
}
```

Since our change continues to delay the terminal phase transition, a Running
pod with early DisruptionTarget will never enter this code path.

**Verdict: SAFE** -- no behavior change.

#### Consumer 2: Taint eviction controller

`addConditionAndDeletePod()` (`pkg/controller/tainteviction/taint_eviction.go:129`)
**sets** DisruptionTarget on a pod (with reason `DeletionByTaintManager`)
and then deletes it. This controller is a **producer** of the condition. It
never reads or reacts to DisruptionTarget set by others.

**Verdict: SAFE** -- only writes the condition.

#### Consumer 3: Device taint eviction controller

Same pattern as the taint eviction controller.
`device_taint_eviction.go:482` **sets** DisruptionTarget with reason
`DeletionByDeviceTaintManager` before deleting the pod.

**Verdict: SAFE** -- only writes the condition.

#### Consumer 4: Pod GC controller

`gc_controller.go:253` **sets** DisruptionTarget on orphaned pods (pods
assigned to deleted nodes, with reason `DeletionByPodGC`) before deleting
them. Producer only.

**Verdict: SAFE** -- only writes the condition.

#### Consumer 5: Scheduler preemption

Two interactions:

- **Producer** (`pkg/scheduler/framework/preemption/executor.go:125`): Sets
  DisruptionTarget with reason `PreemptionByScheduler` on preemption
  victims, then immediately deletes them. This goes through the API server
  directly (not through the kubelet status manager), so our `mergePodStatus`
  change does not affect it.
- **Reader** (`pkg/scheduler/framework/plugins/defaultpreemption/default_preemption.go:401`):
  `podTerminatingByPreemption()` checks for DisruptionTarget with reason
  `PreemptionByScheduler`, but **only after** confirming
  `pod.DeletionTimestamp != nil` (line 402). During kubelet-initiated
  disruption, DeletionTimestamp is not set, so this function returns false
  regardless of DisruptionTarget.

**Verdict: SAFE** -- the producer bypasses the kubelet status manager; the
reader requires DeletionTimestamp which is absent during kubelet disruption.

#### Consumer 6: PDB / Disruption controller -- stale condition cleanup

`nonTerminatingPodHasStaleDisruptionCondition()` (`pkg/controller/disruption/disruption.go:1041`)
monitors for DisruptionTarget conditions that linger on non-terminal pods
without the pod being deleted. After a 2-minute timeout, it resets the
condition to `False`.

**Why it is safe**: The function **explicitly exempts** kubelet-originated
conditions:

```go
// pkg/controller/disruption/disruption.go:1041-1057
func (dc *DisruptionController) nonTerminatingPodHasStaleDisruptionCondition(
    pod *v1.Pod) (bool, time.Duration) {

    if pod.DeletionTimestamp != nil {
        return false, 0
    }
    _, cond := apipod.GetPodCondition(&pod.Status, v1.DisruptionTarget)
    // Pod disruption conditions added by kubelet are never considered stale
    // because the condition might take arbitrarily long before the pod is
    // terminating (has deletion timestamp).
    if cond == nil || cond.Status != v1.ConditionTrue ||
        cond.Reason == v1.PodReasonTerminationByKubelet ||   // <-- exemption
        apipod.IsPodPhaseTerminal(pod.Status.Phase) {
        return false, 0
    }
    waitFor := dc.stalePodDisruptionTimeout - dc.clock.Since(cond.LastTransitionTime.Time)
    if waitFor < 0 { waitFor = 0 }
    return true, waitFor
}
```

DisruptionTarget with reason `TerminationByKubelet` (which is what the
kubelet sets during eviction, shutdown, and preemption) is never considered
stale, regardless of how long it has been present or whether the pod has a
DeletionTimestamp. The PDB controller was designed anticipating that
kubelet-set DisruptionTarget could appear on non-terminal pods.

**Verdict: SAFE** -- kubelet-originated conditions are explicitly exempted
from stale cleanup.

#### Summary

| Consumer | Role | Reads condition? | Guards on terminal phase? | Safe? |
|----------|------|:----------------:|:-------------------------:|:-----:|
| Job controller | Reader | Yes | Yes (`isPodFailed` requires `PodFailed` or deleted) | Yes |
| Taint eviction | Producer | No | N/A | Yes |
| Device taint eviction | Producer | No | N/A | Yes |
| Pod GC | Producer | No | N/A | Yes |
| Scheduler preemption | Both | Yes | Yes (`DeletionTimestamp != nil` required) | Yes |
| PDB controller | Reader | Yes | No, but exempts `TerminationByKubelet` reason | Yes |

**This is the right signal for Phase 1** -- it already exists, is already
set at the right time, every existing consumer is safe with early
propagation, and it just needs to be unblocked and consumed.

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
the existing behavior.

**Current code** (lines 1375-1390):

```go
if c.Type == v1.DisruptionTarget {
    if transitioningToTerminalPhase && !couldHaveRunningContainers {
        updateLastTransitionTime(&newPodStatus, &oldPodStatus, c.Type)
        if _, c := podutil.GetPodConditionFromList(newPodStatus.Conditions, c.Type); c != nil {
            podConditions = statusutil.ReplaceOrAppendPodCondition(podConditions, c)
        }
    }
}
```

**Proposed change**:

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

When the gate is enabled, the `||` short-circuits and DisruptionTarget is
sent immediately, regardless of phase or container state. When the gate is
disabled, the existing condition applies unchanged.

The terminal phase delay (lines 1411-1417) remains untouched:

```go
if transitioningToTerminalPhase {
    if couldHaveRunningContainers {
        newPodStatus.Phase = oldPodStatus.Phase    // keep Running
        newPodStatus.Reason = oldPodStatus.Reason
        newPodStatus.Message = oldPodStatus.Message
    }
}
```

The scheduler resource-reclaim invariant from PR #108366 is preserved.

**Why this is timely enough**: `SetPodStatus()` is called at
`kubelet.go:2315`, before `killPod()` at line 2326. `SetPodStatus` caches
the status and pushes to `podStatusChannel` (`status_manager.go:996`). The
background sync goroutine (`status_manager.go:283`) picks it up and PATCHes
the API server. This goroutine runs concurrently with the pod worker that
executes `killPod()`. So the API server receives the update while containers
are still alive. `mergePodStatus` is the **only gate** preventing early
propagation.

#### 2. Endpoints controllers: consume DisruptionTarget

**Endpointslice controller** (staging library):

**`podToEndpoint()`** (`staging/src/k8s.io/endpointslice/utils.go:38`):
Currently derives `terminating` solely from `DeletionTimestamp`:

```go
serving := endpointutil.IsPodReady(pod)
terminating := pod.DeletionTimestamp != nil
ready := service.Spec.PublishNotReadyAddresses || (serving && !terminating)
```

Change: also set `terminating = true` when DisruptionTarget is present
(controlled by the feature gate, passed via a new parameter):

```go
serving := endpointutil.IsPodReady(pod)
terminating := pod.DeletionTimestamp != nil
if !terminating && disruptionTargetSignalsTerminating {
    terminating = hasDisruptionTargetCondition(pod)
}
ready := service.Spec.PublishNotReadyAddresses || (serving && !terminating)
```

This cascades: when `terminating` is true, `ready` becomes false (unless
`PublishNotReadyAddresses` is set), and the `Terminating` field in the
EndpointSlice is set to true. Downstream consumers (kube-proxy, service
meshes) already handle this field.

**`podEndpointsChanged()`** (`staging/src/k8s.io/endpointslice/util/controller_utils.go:208`):
Add detection of DisruptionTarget condition changes, unconditionally (the
check is cheap -- scanning a short condition list -- and harmless when the
gate is off, since the reconciler will produce the same output):

```go
// After the DeletionTimestamp check (line 218-220), before readiness:
if hasDisruptionTargetCondition(oldPod) != hasDisruptionTargetCondition(newPod) {
    return true, labelsChanged
}
```

**Wire the feature gate via `ReconcilerOption`**: The staging library cannot
import `pkg/features/` (it's a staged module). The existing pattern for this
is `ReconcilerOption` functions. The `Reconciler` struct
(`staging/src/k8s.io/endpointslice/reconciler.go:45`) already uses this
pattern:

```go
// reconciler.go:63-71
type ReconcilerOption func(*Reconciler)

func WithPreferSameTrafficDistributionEnabled(preferSame bool) ReconcilerOption {
    return func(r *Reconciler) {
        r.preferSameTrafficDistribution = preferSame
    }
}
```

Add a matching option:

```go
func WithDisruptionTargetSignalsTerminating(enabled bool) ReconcilerOption {
    return func(r *Reconciler) {
        r.disruptionTargetSignalsTerminating = enabled
    }
}
```

Wire it in `pkg/controller/endpointslice/endpointslice_controller.go:178`,
alongside the existing option:

```go
// endpointslice_controller.go:178-187 (current)
c.reconciler = endpointslicerec.NewReconciler(
    c.client,
    c.nodeLister,
    c.maxEndpointsPerSlice,
    c.endpointSliceTracker,
    c.topologyCache,
    c.eventRecorder,
    ControllerName,
    endpointslicerec.WithPreferSameTrafficDistributionEnabled(
        utilfeature.DefaultFeatureGate.Enabled(features.PreferSameTrafficDistribution)),
    // NEW:
    endpointslicerec.WithDisruptionTargetSignalsTerminating(
        utilfeature.DefaultFeatureGate.Enabled(features.DisruptionTargetSignalsEndpointTerminating)),
)
```

The reconciler already calls `ShouldPodBeInEndpoints(pod, true)` at line 191
(always includes terminating pods) and `podToEndpoint()` at line 228 -- the
new `terminating` logic flows through these existing call sites.

**Legacy endpoints controller** (`pkg/controller/endpoint/endpoints_controller.go`):

`addEndpointSubset()` (line 632) sorts pods into ready vs not-ready subsets.
Currently the ready/not-ready decision depends only on `IsPodReady()`:

```go
// endpoints_controller.go:640
if tolerateUnreadyEndpoints || podutil.IsPodReady(pod) {
    subsets = append(subsets, v1.EndpointSubset{
        Addresses: []v1.EndpointAddress{epa}, Ports: ports,
    })
} else {
    subsets = append(subsets, v1.EndpointSubset{
        NotReadyAddresses: []v1.EndpointAddress{epa}, Ports: ports,
    })
}
```

When the gate is enabled and DisruptionTarget is present, treat the pod
as not-ready regardless of `IsPodReady()`.

#### Feature gate

`DisruptionTargetSignalsEndpointTerminating`, Alpha, default off.

Starting at Alpha (default off) gives the ecosystem one release cycle to
adapt, since controllers and operators may currently assume DisruptionTarget
only appears on pods in a terminal phase.

#### Timeline after Phase 1

```
T+0s   Kubelet receives shutdown signal
T+0s   killPodFunc callback sets DisruptionTarget + Failed phase on status
T+0s   SyncTerminatingPod calls SetPodStatus (kubelet.go:2315)
         -> mergePodStatus: sends DisruptionTarget (gate on), holds phase
         -> updateStatusInternal caches status, signals podStatusChannel
T+0s   Status sync goroutine (status_manager.go:283) picks up signal
         -> syncBatch -> syncPod -> PatchPodStatus to API server
T+~1s  Endpointslice controller sees DisruptionTarget in pod watch
         -> podEndpointsChanged returns true (new check)
         -> podToEndpoint sets Terminating=true, Ready=false
         -> EndpointSlice patched
T+~2s  kube-proxy picks up EndpointSlice change, updates iptables
T+Ns   killPod() finishes: preStop hook runs, SIGTERM sent, containers stop
T+Ns   Final SetPodStatus (kubelet.go:2402): phase now sent too
```

Compare with today:

```
T+0s   Kubelet receives shutdown signal
T+0s   SetPodStatus -> mergePodStatus suppresses EVERYTHING
T+Ns   killPod() finishes, containers stop
T+Ns   Final SetPodStatus -> mergePodStatus sends DisruptionTarget + phase
T+N+1s Endpointslice controller reacts, but pod is dead
```

#### What Phase 1 does NOT cover

- Pods that terminate because their main process exits (RestartNever Jobs).
- Pods that exceed their `activeDeadlineSeconds`.
- Any termination not initiated by infrastructure disruption.

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
| Condition ownership classification | `pkg/kubelet/types/pod_status.go` | 36-58 |
| DisruptionTarget suppression | `pkg/kubelet/status/status_manager.go` | 1375-1390 |
| Terminal phase delay (must preserve) | `pkg/kubelet/status/status_manager.go` | 1411-1417 |
| Status cache + channel signal | `pkg/kubelet/status/status_manager.go` | 993-999 |
| Background sync goroutine | `pkg/kubelet/status/status_manager.go` | 283-294 |
| SetPodStatus entry point | `pkg/kubelet/status/status_manager.go` | 464-488 |
| updateStatusInternal (cache + notify) | `pkg/kubelet/status/status_manager.go` | 849-1012 |
| syncBatch (picks up changes) | `pkg/kubelet/status/status_manager.go` | 1075-1148 |
| SyncTerminatingPod (full sequence) | `pkg/kubelet/kubelet.go` | 2289-2408 |
| First SetPodStatus (before kill) | `pkg/kubelet/kubelet.go` | 2315 |
| killPod (blocks during grace period) | `pkg/kubelet/kubelet.go` | 2326 |
| Final SetPodStatus (after kill) | `pkg/kubelet/kubelet.go` | 2402 |
| Node shutdown sets DisruptionTarget | `pkg/kubelet/nodeshutdown/nodeshutdown_manager.go` | 153-166 |
| Eviction sets DisruptionTarget | `pkg/kubelet/eviction/eviction_manager.go` | 432-438 |
| Kubelet preemption sets DisruptionTarget | `pkg/kubelet/preemption/preemption.go` | 105-116 |
| podToEndpoint (terminating from DeletionTimestamp only) | `staging/src/k8s.io/endpointslice/utils.go` | 38-72 |
| ShouldPodBeInEndpoints | `staging/src/k8s.io/endpointslice/util/controller_utils.go` | 180-197 |
| podEndpointsChanged (no DisruptionTarget check) | `staging/src/k8s.io/endpointslice/util/controller_utils.go` | 208-244 |
| Reconciler struct + ReconcilerOption | `staging/src/k8s.io/endpointslice/reconciler.go` | 45-71 |
| Reconciler calls ShouldPodBeInEndpoints | `staging/src/k8s.io/endpointslice/reconciler.go` | 191 |
| Reconciler calls podToEndpoint | `staging/src/k8s.io/endpointslice/reconciler.go` | 228 |
| NewReconciler wiring | `pkg/controller/endpointslice/endpointslice_controller.go` | 178-187 |
| Legacy addEndpointSubset | `pkg/controller/endpoint/endpoints_controller.go` | 632-655 |
| Job isPodFailed guard | `pkg/controller/job/job_controller.go` | 2169-2179 |
| matchPodFailurePolicy | `pkg/controller/job/pod_failure_policy.go` | 36-82 |
| PDB stale condition exemption | `pkg/controller/disruption/disruption.go` | 1041-1057 |
