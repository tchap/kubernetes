# Implementation Plan: Phase 1 -- DisruptionTarget Signals Endpoint Terminating

## Context

Pods disrupted by the kubelet (graceful node shutdown, eviction, preemption)
are not removed from EndpointSlices before their containers stop. Two things
must change:

1. **Kubelet**: Stop delaying the DisruptionTarget condition -- send it to the
   API server immediately, before containers are killed.
2. **Endpoints controllers**: Consume DisruptionTarget and treat it as a
   termination signal, the same way `deletionTimestamp` is treated today.

See `work/original-proposal.md` for the full problem statement and
`work/assessment.md` for the code-level verification.

---

## Step 1: Add Feature Gate

**File**: `pkg/features/kube_features.go`

Add the constant (alphabetical order, near the `D`s):

```go
// owner: @<owner>
// kep: https://kep.k8s.io/<number>
//
// When enabled, pods with the DisruptionTarget condition are treated as
// terminating by endpoint and endpointslice controllers, and the kubelet
// sends the condition to the API server before containers are stopped.
DisruptionTargetSignalsEndpointTerminating featuregate.Feature = "DisruptionTargetSignalsEndpointTerminating"
```

Add the versioned spec (in the alphabetical map):

```go
DisruptionTargetSignalsEndpointTerminating: {
    {Version: version.MustParse("1.36"), Default: false, PreRelease: featuregate.Alpha},
},
```

**Follows pattern of**: `CRIListStreaming` (constant at line ~121, spec at
line ~1297).

No tests needed -- the feature gate framework is tested generically.

---

## Step 2: Kubelet -- Send DisruptionTarget Early

**File**: `pkg/kubelet/status/status_manager.go`  
**Function**: `mergePodStatus()` (line 1359)

### Current behavior (lines 1375-1390)

DisruptionTarget is suppressed until the pod transitions to a terminal phase
AND all containers have stopped:

```go
if c.Type == v1.DisruptionTarget {
    if transitioningToTerminalPhase && !couldHaveRunningContainers {
        updateLastTransitionTime(...)
        podConditions = statusutil.ReplaceOrAppendPodCondition(podConditions, c)
    }
}
```

### Target behavior

When the feature gate is enabled, send DisruptionTarget immediately -- no
gating on phase or container state. When disabled, keep the existing behavior.
Single body, combined condition:

```go
if c.Type == v1.DisruptionTarget {
    // When the feature gate is enabled, send DisruptionTarget as soon as
    // the kubelet sets it so that endpoints controllers can remove the pod
    // from rotation before containers are stopped. Otherwise, fall back to
    // the original behavior of waiting for terminal phase.
    if utilfeature.DefaultFeatureGate.Enabled(features.DisruptionTargetSignalsEndpointTerminating) ||
        (transitioningToTerminalPhase && !couldHaveRunningContainers) {
        updateLastTransitionTime(&newPodStatus, &oldPodStatus, c.Type)
        if _, c := podutil.GetPodConditionFromList(newPodStatus.Conditions, c.Type); c != nil {
            podConditions = statusutil.ReplaceOrAppendPodCondition(podConditions, c)
        }
    }
}
```

### What NOT to change

The terminal phase delay (lines 1411-1417) MUST remain:

```go
if transitioningToTerminalPhase {
    if couldHaveRunningContainers {
        newPodStatus.Phase = oldPodStatus.Phase    // keep Running
        newPodStatus.Reason = oldPodStatus.Reason
        newPodStatus.Message = oldPodStatus.Message
    }
}
```

This prevents the scheduler from reclaiming resources before containers stop
(invariant from PR #108366). Decoupling DisruptionTarget from this gate is
the entire point.

### Import needed

```go
import (
    utilfeature "k8s.io/apiserver/pkg/util/feature"
    "k8s.io/kubernetes/pkg/features"
)
```

Check if already imported; `status_manager.go` may not currently import
these.

### Tests

**File**: `pkg/kubelet/status/status_manager_test.go`  
**Location**: `TestMergePodStatus` (line ~1659)

Add test cases:

1. **DisruptionTarget sent while phase stays Running (gate enabled)**:
   - `hasRunningContainers: true`, phase attempts Running -> Failed
   - DisruptionTarget in newPodStatus
   - Gate on: expect DisruptionTarget IS in output, phase reverted to Running
   - This is the new behavior -- condition sent early, phase still delayed.

2. **DisruptionTarget still delayed (gate disabled)**:
   - Same setup, gate off
   - Expect DisruptionTarget NOT in output (existing behavior preserved)

---

## Step 3: Endpoint Change Detection

**File**: `staging/src/k8s.io/endpointslice/util/controller_utils.go`  
**Function**: `podEndpointsChanged()` (line 208)

### Why this must change

`podEndpointsChanged()` is called from `GetPodUpdateProjectionKey()` (line
84) to decide whether a pod update should trigger endpoint reconciliation.
Currently it detects `DeletionTimestamp` changes, readiness changes, and IP
changes. If DisruptionTarget appears on a pod and nothing else changes, the
endpointslice controller will not reconcile.

### Design choice: always detect, don't gate

This function lives in the staging library which cannot import
`pkg/features/`. But the check is cheap (scan a short condition list), and
detecting the change unconditionally is harmless -- if the feature gate is
off, the reconciler will produce the same EndpointSlice output regardless.
Only the reconciler needs the gate.

### Change

Add after the DeletionTimestamp check (line 218-220) and before the
readiness check:

```go
// If the pod's DisruptionTarget condition has changed, the endpoint may
// need to be marked as terminating.
if hasDisruptionTargetCondition(oldPod) != hasDisruptionTargetCondition(newPod) {
    return true, labelsChanged
}
```

### Helper function (same file)

```go
// hasDisruptionTargetCondition returns true if the pod has the
// DisruptionTarget condition set to True.
func hasDisruptionTargetCondition(pod *v1.Pod) bool {
    for _, c := range pod.Status.Conditions {
        if c.Type == v1.DisruptionTarget && c.Status == v1.ConditionTrue {
            return true
        }
    }
    return false
}
```

### Tests

**File**: `staging/src/k8s.io/endpointslice/util/controller_utils_test.go`  
**Location**: `TestPodEndpointsChanged` (line ~520)

Add modifier for DisruptionTarget condition (similar to existing "mark for
deletion" modifier at line ~551). Test that appearance of DisruptionTarget
returns `podChanged=true`.

---

## Step 4: EndpointSlice -- Mark Disrupted Pods as Terminating

**File**: `staging/src/k8s.io/endpointslice/utils.go`  
**Function**: `podToEndpoint()` (line 38)

### Current code

```go
func podToEndpoint(pod *v1.Pod, node *v1.Node, service *v1.Service, addressType discovery.AddressType) discovery.Endpoint {
    serving := endpointutil.IsPodReady(pod)
    terminating := pod.DeletionTimestamp != nil
    ready := service.Spec.PublishNotReadyAddresses || (serving && !terminating)
    ...
}
```

### Change

Add a parameter to carry the feature flag, and expand the `terminating`
condition:

```go
func podToEndpoint(pod *v1.Pod, node *v1.Node, service *v1.Service, addressType discovery.AddressType, disruptionTargetSignalsTerminating bool) discovery.Endpoint {
    serving := endpointutil.IsPodReady(pod)
    terminating := pod.DeletionTimestamp != nil
    if !terminating && disruptionTargetSignalsTerminating {
        terminating = hasDisruptionTargetCondition(pod)
    }
    ready := service.Spec.PublishNotReadyAddresses || (serving && !terminating)
    ...
}
```

The `hasDisruptionTargetCondition` helper is in the `util/` subpackage; add
an exported version or duplicate the 5-line helper in this file (preferred,
to avoid a circular or awkward import -- the existing `IsPodReady` is already
copied the same way).

### Caller update

**File**: `staging/src/k8s.io/endpointslice/reconciler.go` (line 228)

```go
// Before:
endpoint := podToEndpoint(pod, node, service, addressType)

// After:
endpoint := podToEndpoint(pod, node, service, addressType, r.disruptionTargetSignalsTerminating)
```

### Tests

**File**: `staging/src/k8s.io/endpointslice/utils_test.go`

Add test cases in the existing `TestPodToEndpoint` or similar:

1. Pod with DisruptionTarget=True, no DeletionTimestamp, flag enabled:
   - Expect `Terminating=true`, `Ready=false`, `Serving=<actual readiness>`
2. Same pod, flag disabled:
   - Expect `Terminating=false`, `Ready=<actual readiness>`

---

## Step 5: Wire Feature Gate into Reconciler

**File**: `staging/src/k8s.io/endpointslice/reconciler.go`

Add field to `Reconciler` struct (line 45):

```go
type Reconciler struct {
    ...
    disruptionTargetSignalsTerminating bool
    ...
}
```

Add option function (after `WithPreferSameTrafficDistributionEnabled`, line
67):

```go
// WithDisruptionTargetSignalsTerminating controls whether the Reconciler
// treats pods with the DisruptionTarget condition as terminating.
func WithDisruptionTargetSignalsTerminating(enabled bool) ReconcilerOption {
    return func(r *Reconciler) {
        r.disruptionTargetSignalsTerminating = enabled
    }
}
```

**File**: `pkg/controller/endpointslice/endpointslice_controller.go` (line
186)

Wire it alongside the existing option:

```go
c.reconciler = endpointslicerec.NewReconciler(
    c.client,
    c.nodeLister,
    c.maxEndpointsPerSlice,
    c.endpointSliceTracker,
    c.topologyCache,
    c.eventRecorder,
    ControllerName,
    endpointslicerec.WithPreferSameTrafficDistributionEnabled(utilfeature.DefaultFeatureGate.Enabled(features.PreferSameTrafficDistribution)),
    endpointslicerec.WithDisruptionTargetSignalsTerminating(utilfeature.DefaultFeatureGate.Enabled(features.DisruptionTargetSignalsEndpointTerminating)),
)
```

No dedicated tests -- the wiring is exercised by the integration test below.

---

## Step 6: Legacy Endpoints Controller

**File**: `pkg/controller/endpoint/endpoints_controller.go`

The legacy controller calls `ShouldPodBeInEndpoints()` at line 396 (which
excludes terminating pods when `PublishNotReadyAddresses` is false) and
`addEndpointSubset()` at line 640 (which sorts into ready vs not-ready).

### Change to `addEndpointSubset` (line 632)

When the feature gate is enabled and the pod has DisruptionTarget, treat
it as not ready regardless of `IsPodReady()`:

```go
func addEndpointSubset(logger klog.Logger, subsets []v1.EndpointSubset, pod *v1.Pod, epa v1.EndpointAddress,
    epp *v1.EndpointPort, tolerateUnreadyEndpoints bool) ([]v1.EndpointSubset, int, int) {
    ...
    isReady := podutil.IsPodReady(pod)
    if utilfeature.DefaultFeatureGate.Enabled(features.DisruptionTargetSignalsEndpointTerminating) {
        if hasDisruptionTargetCondition(pod) {
            isReady = false
        }
    }
    if tolerateUnreadyEndpoints || isReady {
    ...
```

The `hasDisruptionTargetCondition` helper can be imported from the
endpointslice util package or duplicated here (it's 5 lines).

Note: the legacy Endpoints API is deprecated but still active. This
change ensures consistent behavior across both controllers.

### Tests

**File**: `pkg/controller/endpoint/endpoints_controller_test.go`

Test that a ready pod with DisruptionTarget is placed in `NotReadyAddresses`
when the gate is enabled, and in `Addresses` when the gate is disabled.

---

## Step 7: Integration Test

**File**: `test/integration/endpointslice/endpointsliceterminating_test.go`

Add a new test function `TestEndpointSliceDisruptionTargetTerminating`:

1. Create a service and a ready pod.
2. Verify the pod appears in EndpointSlice with `Ready=true`.
3. Patch the pod to add `DisruptionTarget=True` condition (simulating what
   the kubelet does during eviction/shutdown).
4. Poll until EndpointSlice shows `Terminating=true`, `Ready=false` for
   the pod.
5. Verify `Serving` is still true (pod is technically still serving, just
   being disrupted).

This exercises the full pipeline: change detection (Step 3), endpoint
translation (Step 4), reconciler wiring (Step 5).

---

## File Summary

| File | Change |
|------|--------|
| `pkg/features/kube_features.go` | Add `DisruptionTargetSignalsEndpointTerminating` gate |
| `pkg/kubelet/status/status_manager.go` | Send DisruptionTarget immediately when gate enabled |
| `pkg/kubelet/status/status_manager_test.go` | Test early sending with gate on/off |
| `staging/src/k8s.io/endpointslice/util/controller_utils.go` | `podEndpointsChanged()`: detect DisruptionTarget changes; add helper |
| `staging/src/k8s.io/endpointslice/util/controller_utils_test.go` | Test change detection |
| `staging/src/k8s.io/endpointslice/utils.go` | `podToEndpoint()`: mark disrupted pods terminating |
| `staging/src/k8s.io/endpointslice/utils_test.go` | Test `podToEndpoint` with DisruptionTarget |
| `staging/src/k8s.io/endpointslice/reconciler.go` | Add `disruptionTargetSignalsTerminating` field + option |
| `pkg/controller/endpointslice/endpointslice_controller.go` | Wire gate into reconciler option |
| `pkg/controller/endpoint/endpoints_controller.go` | Treat disrupted pods as not-ready |
| `pkg/controller/endpoint/endpoints_controller_test.go` | Test legacy controller behavior |
| `test/integration/endpointslice/endpointsliceterminating_test.go` | Integration test with DisruptionTarget |

---

## Risks and Mitigations

### Job controller sees early DisruptionTarget

The job controller uses DisruptionTarget for pod failure policy retry
decisions. Sending it early could cause premature retries.

**Mitigation**: Verified that the job controller only evaluates
DisruptionTarget on pods that have reached a terminal phase. Since we
continue to delay the terminal phase transition, the job controller will not
see DisruptionTarget on a Running pod. No behavior change for jobs.

### Ecosystem consumers assume DisruptionTarget = terminal

Some operators may assume DisruptionTarget only appears on terminal pods
(since that has been the behavior since GA).

**Mitigation**: The feature gate defaults to off (Alpha). One release cycle
of Alpha gives the ecosystem time to adapt before Beta (default on).

### `podToEndpoint` signature change

Adding a parameter to `podToEndpoint` is a breaking change to the staging
library's internal API.

**Mitigation**: The function is unexported (lowercase `p`). It is only
called from `reconcileByAddressType` within the same package. No external
callers.

---

## Verification

1. **Unit tests**: `go test ./pkg/kubelet/status/ ./staging/src/k8s.io/endpointslice/... ./pkg/controller/endpoint/ -run "TestMergePodStatus|TestPodToEndpoint|TestPodEndpointsChanged|TestEndpointSubset"`
2. **Integration tests**: `go test ./test/integration/endpointslice/ -run "TestEndpointSliceDisruptionTargetTerminating|TestEndpointSliceTerminating" -v`
3. **Manual verification**: Use a kind cluster with the feature gate enabled. Evict a pod (`kubectl drain`) or trigger node shutdown, and observe that the EndpointSlice marks the pod as `Terminating=true` before the containers stop.
