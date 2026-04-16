# Assessment: Disrupted Pods Should Be Removed from Endpoints

Reviewer: Claude (Opus 4.6), acting as senior Kubernetes architect  
Date: 2026-04-16  
Reviewed against: kubernetes/kubernetes @ commit 127f3c0e117 (branch kep-endpoints-on-shutdown)

---

## Executive Summary

The proposal correctly identifies a real and impactful gap in Kubernetes: pods
undergoing kubelet-initiated disruption (graceful node shutdown, eviction,
preemption) are **not removed from endpoints** before their containers stop.
The root cause analysis is accurate, the proposed phased approach is sound, and
the alternatives section is thorough. This assessment validates each claim
against the codebase and raises several implementation concerns that the KEP
should address before proceeding.

**Verdict: The proposal should proceed, with the refinements noted below.**

---

## 1. Verification of Claims Against the Codebase

### 1.1 "The endpoints controller already uses deletionTimestamp as a signal to eagerly remove pods from endpoints" -- CONFIRMED

`ShouldPodBeInEndpoints()` in
`staging/src/k8s.io/endpointslice/util/controller_utils.go:180-197` explicitly
checks `pod.DeletionTimestamp != nil` at line 192. If `includeTerminating` is
false and `DeletionTimestamp` is set, the pod is excluded.

In the EndpointSlice path, `podToEndpoint()` at
`staging/src/k8s.io/endpointslice/utils.go:40` derives the `terminating`
boolean exclusively from `pod.DeletionTimestamp != nil`. The `ready` field is
then set to `false` when `terminating` is true (line 43), unless
`PublishNotReadyAddresses` is set.

**Implication confirmed:** Pods that are never _deleted_ (only disrupted) will
never have `DeletionTimestamp` set, and therefore the existing endpoints
machinery has no signal to remove them.

### 1.2 "EndpointSlices report a terminating boolean per endpoint" -- CONFIRMED

`utils.go:49` sets `Terminating: &terminating` in `EndpointConditions`. The
field is defined in `staging/src/k8s.io/api/discovery/v1/types.go`. The
reference to the Cilium PR in the proposal is slightly misleading since this is
an upstream Kubernetes API field, but the claim itself is correct.

### 1.3 "The [DisruptionTarget] condition is currently delayed until the final status is written" -- CONFIRMED, and this is the crux of the problem

`mergePodStatus()` in `pkg/kubelet/status/status_manager.go:1375-1390`
explicitly gates sending DisruptionTarget to the API server:

```go
if transitioningToTerminalPhase && !couldHaveRunningContainers {
    // only now send DisruptionTarget
}
```

The full sequence in `SyncTerminatingPod` (`pkg/kubelet/kubelet.go:2289-2408`)
is:

1. **Line 2311-2315**: Generate API status, apply the `podStatusFn` callback
   (which sets DisruptionTarget + terminal phase), call `SetPodStatus` --
   **but `mergePodStatus` suppresses both the phase transition and
   DisruptionTarget because containers are still running.**
2. **Line 2323**: Stop liveness and startup probes.
3. **Line 2326**: `killPod()` -- run preStop hooks, send SIGTERM, wait for
   grace period. **Containers stop here.**
4. **Line 2336**: Remove all probes (including readiness).
5. **Line 2401-2402**: Generate final status, call `SetPodStatus` again --
   **this time `couldHaveRunningContainers` is false, so DisruptionTarget
   and the terminal phase are finally sent to the API server.**

**Bottom line**: By the time DisruptionTarget reaches the API server, the
containers are already stopped. Even if the endpoints controller consumed
DisruptionTarget today, it would arrive too late to be useful.

### 1.4 "DisruptionTarget is not consumed by endpoints/endpointslice controllers" -- CONFIRMED

Grep across `pkg/controller/endpoint/`, `pkg/controller/endpointslice/`,
`pkg/controller/endpointslicemirroring/`, and
`staging/src/k8s.io/endpointslice/` returns zero matches for
`DisruptionTarget`. The condition is consumed only by:

- Job controller (for pod failure policy retry decisions)
- Taint eviction controller
- Pod GC controller
- Scheduler preemption (checking for `PodReasonPreemptionByScheduler`)

### 1.5 "Pod readiness is a trailing indicator of pod disruption" -- CONFIRMED

- Readiness probes continue running during `SyncTerminatingPod` until
  `RemovePod()` is called **after** containers stop (line 2336).
- During eviction, the phase is set to `Failed` internally before containers
  stop, which causes probe workers to exit early (issue #124648).
- Pods without readiness probes are always considered ready; there is no
  mechanism to flip them to unready during termination.

### 1.6 "PodDisruptionConditions is GA" -- CONFIRMED

- Graduated to GA and locked in v1.31 (CHANGELOG-1.31.md).
- Feature gate removed entirely in v1.34 (CHANGELOG-1.34.md).
- DisruptionTarget is now always enabled.

### 1.7 "PendingTermination condition does not exist yet" -- CONFIRMED

Grep for `PendingTermination` returns matches only in
`work/original-proposal.md`. No API type, feature gate, or controller code
exists for this condition.

---

## 2. Assessment of the Linked Issues and PR

| Reference | Status | Relevance |
|-----------|--------|-----------|
| [#116965](https://github.com/kubernetes/kubernetes/issues/116965) - Graceful node shutdown does not update endpoints | Open | **Primary motivating issue.** Confirms the problem is real and unresolved. |
| [#124648](https://github.com/kubernetes/kubernetes/issues/124648) - Readiness probe stops too early at eviction | Open | **Secondary issue.** Phase set to Failed before containers stop causes probes to exit. |
| [#125774](https://github.com/kubernetes/kubernetes/pull/125774) - POC: DisruptionTarget signals terminating | Closed (rotten) | **POC went stale.** Never merged, but validates the approach is implementable. Introduced feature gate `PodDisruptionConditionSignalsTerminating`. |
| [#108366](https://github.com/kubernetes/kubernetes/pull/108366) - Delay terminal phase until pod terminated | Merged | **Root cause of the delay.** Intentionally delays phase+DisruptionTarget to prevent scheduler resource reclaim races. Any fix must preserve this invariant. |
| [#105780](https://github.com/kubernetes/kubernetes/issues/105780) - Readiness probes should keep running during shutdown | Open | **Related bug.** All probes disabled during graceful node termination; pods remain "Ready" throughout shutdown. |

**Key insight from #108366**: The delay of DisruptionTarget is not accidental --
it was introduced to prevent the scheduler from reclaiming resources before
containers actually stop. The proposal must not break this invariant. The fix
should decouple sending DisruptionTarget to the API server from delaying the
terminal phase transition.

---

## 3. Assessment of the Proposed Approach

### Phase 1: Eagerly propagate DisruptionTarget

**Strengths:**

- Leverages an existing GA condition -- no new API types needed.
- DisruptionTarget is already set in the `podStatusFn` callback before
  containers stop; the only change is removing the suppression in
  `mergePodStatus`.
- Endpoints/endpointslice controllers already have machinery to watch pod
  conditions; adding a check for DisruptionTarget is straightforward.
- Feature-gated but defaulted on is the right call since the condition is
  already GA.

**Concerns and recommendations:**

1. **Decoupling DisruptionTarget from terminal phase delay.** The current
   `mergePodStatus` delays _both_ the phase transition and DisruptionTarget
   together (lines 1375-1390). The proposal should explicitly state that:
   - DisruptionTarget should be sent to the API as soon as it is set,
     _independently_ of the phase transition.
   - The terminal phase delay (lines 1411-1415) must remain intact to
     preserve the scheduler resource reclaim invariant from PR #108366.
   - This means splitting the condition in `mergePodStatus`: remove the
     `transitioningToTerminalPhase && !couldHaveRunningContainers` gate for
     DisruptionTarget while keeping it for the phase.

2. **Impact on job controller.** The job controller uses DisruptionTarget to
   decide whether a pod failure is retriable. Today it only sees
   DisruptionTarget on pods that are already in a terminal phase. If
   DisruptionTarget arrives while the pod is still `Running`, the job
   controller might attempt a premature retry. **Recommendation:** Verify
   that the job controller's pod failure policy logic only triggers on
   terminal phase + DisruptionTarget, not on DisruptionTarget alone. From
   inspection of `pkg/controller/job/pod_failure_policy.go`, the job
   controller checks conditions on pods that have reached a terminal phase,
   so this should be safe -- but the KEP should explicitly call out this
   analysis.

3. **EndpointSlice `terminating` field semantics.** Currently `terminating`
   is derived solely from `DeletionTimestamp` (utils.go:40). The proposal
   should specify whether:
   - `terminating` should also be set to `true` when DisruptionTarget is
     present, OR
   - A new field or condition-based check should be added.
   
   Setting `terminating = true` from DisruptionTarget would be the cleanest
   approach since downstream consumers (kube-proxy, service mesh, etc.)
   already handle this field.

4. **Race window.** Even with eager DisruptionTarget, there is an inherent
   race between:
   - Kubelet sets DisruptionTarget in the API
   - Endpoints controller reacts and updates EndpointSlice
   - kube-proxy/load balancer picks up the EndpointSlice change
   - Container actually stops
   
   The proposal should discuss the expected timing and note that the preStop
   hook / SIGTERM grace period provides the window for this propagation.
   Workloads without preStop hooks or SIGTERM handling will still see a
   reduced (but nonzero) window compared to today.

5. **`podEndpointsChanged()` needs updating.** The function at
   `staging/src/k8s.io/endpointslice/util/controller_utils.go:208-244`
   detects endpoint-relevant pod changes. It currently checks
   `DeletionTimestamp` and readiness changes. It must also detect
   DisruptionTarget condition changes to trigger endpoint reconciliation.

### Phase 2: PendingTermination condition

**Strengths:**

- Covers scenarios that DisruptionTarget cannot: container exit on
  RestartNever/RestartOnFailure pods, active deadline exceeded, and other
  non-disruption terminations.
- Orthogonal to DisruptionTarget -- does not require changing DisruptionTarget
  semantics.
- The "remove if source of termination was not durable" behavior on kubelet
  restart is important for correctness after transient disruptions.

**Concerns and recommendations:**

6. **Scope definition needs tightening.** The proposal is vague about exactly
   when PendingTermination is set for non-disruption scenarios:
   - For RestartNever pods where a container exits with code 0: when does
     the condition get set? At container exit? At the start of
     SyncTerminatingPod?
   - For RestartOnFailure pods where a container fails: is PendingTermination
     set even though the container will be restarted?
   - For ActiveDeadlineSeconds: is it set when the deadline fires or when
     container termination begins?
   
   **Recommendation:** Define PendingTermination as "the kubelet pod worker
   has entered the terminating state for this pod" -- i.e., the pod worker's
   `startedTerminating` flag is true. This is unambiguous and covers all
   scenarios.

7. **Interaction with RestartAlways pods.** The proposal's appendix discusses
   that graceful node shutdown should perhaps not drive RestartAlways pods to
   terminal phases. If PendingTermination is set for these pods during
   shutdown, but they are expected to restart after reboot, the condition
   must be reliably removed. The proposal says "removed if the Kubelet
   restarts and the source of the termination was not durable" but this
   requires the kubelet to persist or reconstruct this knowledge after
   reboot. Since the kubelet has no local store, this needs careful design.

8. **New API surface area.** PendingTermination is a new PodConditionType.
   This requires:
   - API review and approval
   - A new feature gate (the proposal names it `PodPendingTerminationCondition`)
   - Alpha/Beta/GA graduation
   - Documentation
   
   This is a longer path than Phase 1 and should be planned on a separate
   timeline.

---

## 4. Gaps and Additional Recommendations

### 4.1 The proposal should address the readiness probe bugs it identifies

Issues #105780 and #124648 are real bugs that compound the endpoints problem.
While the proposal correctly notes them, fixing them independently would
provide immediate partial relief:

- **#105780**: Pods should be marked not-ready (or probes should continue)
  during graceful node shutdown. This alone would cause some endpoints
  consumers to remove pods.
- **#124648**: Readiness probes exiting early during eviction means even pods
  _with_ readiness probes don't get removed from endpoints during eviction.

**Recommendation:** The KEP should either include fixing these bugs as part of
Phase 1 or explicitly note them as parallel work items.

### 4.2 Consider a minimal Phase 0

Before the full Phase 1 implementation, a targeted fix could address the most
acute problem (graceful node shutdown) without modifying `mergePodStatus`:

- The node shutdown manager could directly delete pods after initiating
  shutdown, with an opt-in flag.
- This is listed as an alternative but dismissed. The dismissal is valid for
  the general case, but for environments where nodes are known to be
  transient (cloud spot instances), this could be an immediate mitigation.

### 4.3 The proposal should discuss backwards compatibility of early DisruptionTarget

Some controllers or operators in the ecosystem might assume that
DisruptionTarget only appears on pods in a terminal phase (since that has been
the behavior since GA). The KEP should:

- Survey known consumers beyond the core controllers.
- Consider whether the feature gate should default to off initially for one
  release to allow ecosystem adaptation.

### 4.4 Testing strategy

The KEP should outline:

- Integration tests that verify endpoints are updated before containers stop
  during graceful node shutdown, eviction, and preemption.
- E2E tests that measure the latency between disruption signal and endpoint
  removal.
- Regression tests ensuring job controller behavior is unchanged.

---

## 5. Summary of Findings

| # | Proposal Claim | Verified? | Notes |
|---|---------------|-----------|-------|
| 1 | deletionTimestamp used to remove from endpoints | Yes | `controller_utils.go:192`, `utils.go:40` |
| 2 | EndpointSlices report terminating boolean | Yes | `utils.go:49`, derived from DeletionTimestamp only |
| 3 | DisruptionTarget delayed until final status | Yes | `status_manager.go:1375-1390` -- **this is the core bug** |
| 4 | DisruptionTarget not consumed by endpoints controllers | Yes | Zero matches in endpoints controller packages |
| 5 | Pod readiness is trailing indicator | Yes | Probes run until after containers stop |
| 6 | PodDisruptionConditions is GA | Yes | GA in v1.31, gate removed in v1.34 |
| 7 | Graceful node shutdown sets DisruptionTarget | Yes | `nodeshutdown_manager.go:160-166` |
| 8 | Eviction sets DisruptionTarget | Yes | `eviction_manager.go:432-438` |

---

## 6. Recommended Changes to the Proposal

1. **Add explicit detail on how `mergePodStatus` will be modified.** The key
   change is allowing DisruptionTarget to be sent independently of the
   terminal phase gate. This is the single most important implementation
   detail and it is currently implicit.

2. **Add analysis of job controller impact.** Show that the job controller
   only acts on DisruptionTarget for pods in terminal phase, so early
   propagation is safe.

3. **Specify how `podToEndpoint()` and `ShouldPodBeInEndpoints()` will
   consume DisruptionTarget.** The `terminating` field in EndpointSlice
   should be true when either `DeletionTimestamp != nil` OR
   `DisruptionTarget == True`. Similarly, `ShouldPodBeInEndpoints` should
   exclude pods with DisruptionTarget when `includeTerminating` is false.

4. **Specify how `podEndpointsChanged()` will detect DisruptionTarget
   changes** to trigger endpoint reconciliation.

5. **Tighten the definition of PendingTermination** to "the kubelet pod
   worker has entered the terminating state," and define the behavior for
   each restart policy.

6. **Address the readiness probe bugs** (#105780, #124648) as either
   in-scope or explicitly parallel work.

7. **Add a testing strategy section.**

8. **Add a timeline** separating Phase 1 (can target the next release cycle)
   from Phase 2 (requires new API, longer graduation path).
