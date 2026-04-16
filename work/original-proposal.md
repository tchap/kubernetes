# Disrupted Pods should be removed from endpoints

Reported in [issue 116965](https://github.com/kubernetes/kubernetes/issues/116965) and [issue 124648](https://github.com/kubernetes/kubernetes/issues/124648)  
Proof of concept in [PR 125774](https://github.com/kubernetes/kubernetes/pull/125774)  
All members of [dev@kubernetes.io](mailto:dev@kubernetes.io) can comment  
Reviewed with mimowo@, aojea@, bobbypage@, yujuhong@  
Shared with sig-node, sig-network, and sig-apps

## Problem

Pods that are located on nodes undergoing [graceful node shutdown](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/2000-graceful-node-shutdown) or non-immediate eviction have no chance to be removed from load balancers before the pods terminate, leading to client-visible errors \- traffic is directed to instances that are no longer running resulting in connection denied or other higher level protocol errors.

## Cause

There is no signal sent by the Kubelet across all distributions during graceful node shutdown or other sources of abnormal workload termination that results in the pod being removed from endpoints before the containers in the pod are stopped.

### Why do we need to remove pods from endpoints before they stop?

Load balancers that read from endpoints are generally eventually consistent \- they have some latency between when a pod is removed from EndpointSlices and when all configuration is updated \- this ranges from O(1s) for kube-proxy to O(60s) for some service load balancer implementations or DNS.

Pods that are terminated by workload controllers delay the termination of the container via the preStop hook or by intercepting SIGTERM to drain traffic for a time window that is set by the workload to allow this signal to propagate.  The drain time is often experimentally determined for a particular environment and having more time to drain is always better.

**Kubernetes Principle**: Pods that are known to be disrupted in the future should be eagerly removed from endpoints so that eventually consistent load balancers have time to propagate the removal.  Readiness is a trailing indicator, whereas for this problem we need a leading indicator.

## Proposed high-level solution

We should send an unambiguous and early signal when pods will be terminated for any reason beyond their natural lifecycle that allows the endpoints controller to remove the pod from rotation.

**Kubernetes Principle:** Pods should behave consistently whether they are gracefully terminated by the control plane (deletionTimestamp set), gracefully disrupted by the Kubelet (eviction due to soft thresholds), or forcibly disrupted by the Kubelet (eviction due to hard thresholds).

### Potential signals

#### Pod readiness (condition PodReady is True)

When a pod is ready, endpoints consumers are expected to direct traffic to it. A pod without readiness probes is always ready. A pod is marked unready once the Kubelet reports a terminal phase (Succeeded or Failed), which after 1.22 only [occurs once all containers are already stopped](https://github.com/kubernetes/kubernetes/pull/108366).

Pod readiness is a trailing indicator of pod disruption \- readiness changes after probes fail. Graceful node shutdown must work for all workloads on a node and the signal must be a leading indicator. For readiness to be used as a leading indicator, graceful node shutdown would have to set the PodReady condition to false during pod termination and override the readiness probe status provided by the workload. This would not be an acceptable solution because there are valid scenarios where a workload can remain ready post termination and overriding readiness would break those scenarios.

**Kubernetes Principle:** Pod readiness signal should remain orthogonal to other disruptions to the workload because readiness is a user defined signal that may be consumed directly by higher level components beyond networking.

#### Pod is gracefully deleted (deletionTimestamp set)

When a pod is known to be unnecessary, an API consumer may DELETE the pod which triggers a graceful termination on the Kubelet.  The deletion request sets deletionTimestamp not nil, but does not actually delete the pod.  A pod undergoing graceful termination is guaranteed to eventually be removed on the Kubelet unless the node is partitioned \- in which case the node controller or admin delivers a signal that results in the pod going unready.

The endpoints controller already uses deletionTimestamp set as a signal to eagerly remove pods from endpoints. In endpoint slices, we implement that behavior and also [report a terminating boolean per endpoint](https://github.com/cilium/cilium/pull/24174) which allows sophisticated clients to discriminate between unready pods and terminating but ready pods.

Graceful node shutdown cannot use pod deletion as a signal of unreadiness because it is a mechanism for many kinds of node disruption where the pod is only temporarily impacted.  For nodes that are intentionally transient (spot VMs) where shutdown signals the end of the pod’s lifecycle, deleting the pods eagerly may be appropriate.  However, because Kubernetes cannot safely determine whether a node shutdown is permanent or not via the existing shutdown signals, Kubernetes cannot safely delete pods on shutdown.  If we implement a better signal it will not be necessary to delete these pods from the node.

**Kubernetes Principle:** Pods should not be deleted by Kubernetes controllers unless the user has opted in to the behavior (via a workload controller, an admin deleting a node, or a higher level operational concern like automated node upgrades draining the node)

Potential bug: not all graceful node shutdowns should result in pods being driven to terminal phases (for RestartAlways and RestartOnFailure pods) if the reboot is a temporary disruption.

#### Pod is the target of disruption (condition DisruptionTarget is True)

[Pod failure policy](https://github.com/kubernetes/enhancements/tree/master/keps/sig-apps/3329-retriable-and-non-retriable-failures) is a GA in 1.31 feature that introduces a new condition to pods indicating the pods will be the target of disruption at a future point.  The condition may be set through the API by controllers or by the Kubelet when some forms of pod disruption are anticipated.  This includes graceful node shutdown, preemption, and some eviction scenarios. The condition is only set for disruptions where we are confident the workload itself did not result in the disruption to ensure that job controllers can safely retry those pods.

The condition is currently delayed until the final status is written which prevents it from providing a leading signal. mimowo@ suggested that [part of the design](https://github.com/kubernetes/enhancements/tree/master/keps/sig-apps/3329-retriable-and-non-retriable-failures#new-podconditions) could be relaxed. However, as the KEP documents only a [subset of sources of pod disruption](https://github.com/kubernetes/enhancements/tree/master/keps/sig-apps/3329-retriable-and-non-retriable-failures#current-state-review) are suitable for inclusion in this condition without breaking the intent of allowing job controllers to safely restart the pod.  Therefore, we need an additional signal to cover those scenarios.

#### Pod is pending termination (new condition PendingTermination is True)

A condition that signaled the Kubelet was beginning termination of a pod would provide valuable info to admins for Kubelet initiated disruption such as eviction, active deadline seconds exceeded (which begins termination at the deadline but allows graceful shutdown), as well as complementing pod failure policy by exposing reasons that can be correlated to specific failure types.  The condition could also be used to signal when all pod resources are fully released which currently cannot be detected via the API (see [KEP 4577](https://github.com/kubernetes/enhancements/pull/4577) for a proposal which addresses this specific problem vs the broader endpoints problem).

Implementing this signal would require a new condition which is a new API.  If a reboot occurs and the reason for the termination is no longer detected, the condition should be removed.  A pod that is in a terminal phase and has the condition should set the value from True to False when all pod resources are released.

## Proposed Implementation

### Phase 1: Pods are eagerly excluded due to DisruptionTarget

The Kubelet would be updated to propagate the DisruptionTarget to the API as soon as the pod begins terminating rather than waiting.

The endpoint and endpointslice controllers will leverage the DisruptionTarget condition to indicate that a pod will be terminating in the future and that they should take those pods out of rotation eagerly.  On endpointslices, the pod should have the terminating boolean true if the condition is true.  If the condition is removed or set to False, the endpoint should be in rotation as long as it is still Ready.  This behavior would be feature gated but defaulted on so that clusters with DisruptionTarget already set for graceful node shutdown can see an improvement.

### Phase 2: Pods are eagerly excluded on any termination

An existing KEP / new KEP will add details of the PendingTermination condition and fully specify the behavior.  The condition will be set for all pods when the Kubelet state machine begins termination for any reason.  The condition will be removed if the Kubelet restarts and the source of the termination was not durable (was not a deletionTimestamp).  The condition will be consumed by endpoint and endpointslice controllers \- if either DisruptionTarget or PendingTermination is set to true, the pod will be considered “terminating” in the endpoint and thus excluded from rotation.  A new feature gate will control setting and reading that condition (PodPendingTerminationCondition) at the appropriate stability level.

The presence of this condition allows all pods \- those disrupted via DisruptionTarget, those that are disrupted via mechanisms that cannot be added to DisruptionTarget because they may be caused by the workload, and RestartNever/RestartOnFailure pods that terminate due to container exit \- to be eagerly removed.

## Alternatives Considered

* Delete pods during graceful node shutdown  
  * Deleting a pod removes info about the pods final status which some human users may depend on  
  * Pods should be removed from endpoints on any disruption, not just graceful node shutdown  
  * Some clusters run in environments where nodes don’t go away on shutdown  
  * A node may be stopped for many reasons and even spot VMs may support a restart option  
  * Some workloads may prefer to remain running on a given node with local caches past restart  
* Override pod readiness for terminating pods to False  
  * Suppresses the correct value from the pod readiness probe and breaks orthogonality of readiness and termination which is part of the public API for EndpointSlices  
  * Would block users who use readiness checks during graceful deletion (especially while preStop hooks are running) and thus break compatibility of behavior  
* Change DisruptionTarget definition to include new disruption types  
  * Backward-incompatible change to job controller retry that breaks the intent of DisruptionTarget  
* Add a new “dual” condition of DisruptionTarget to cover all scenarios where the pod \*might\* be the source of the disruption  
  * Not needed by the DisruptionTarget consumer (any condition is valuable, but a dual is not specifically needed)  
  * Having PendingTermination orthogonal allows us to benefit DisruptionTarget but support knowing when the pod is completely cleaned up  
  * No strong need to couple the new condition to DisruptionTarget and manage overlap.  PendingTermination always set  
* Do nothing; allow graceful node shutdown to ungracefully disrupt running workloads  
  * Limits use of graceful node shutdown to workloads that are disrupted  
  * Makes behavior of graceful node shutdown distribution dependent

## Appendix

### Graceful node shutdown should probably not fully stop some pods

Not all graceful node shutdowns will result in the removal of the node.  The current default implementation is geared towards nodes that will not return after reboot (e.g. cloud spot instances) or nodes where all the user workload pods have been drained (on-premise Kubernetes implementations that manage node lifecycle e.g. OpenShift).  However, graceful node shutdown should work for all Kubernetes distributions and use cases, and it is reasonable to want graceful shutdown of a node so that pods are left in place yet still gracefully removed from rotations.

The Kubelet provides a best effort signal to the control plane of status, and as it has no local store of state by design if shutdown completes without propagation of pod phase the appropriate behavior is always to restart the pod \- we do not provide a guarantee of “at most once” execution of pod containers on the nodes.

To solve this, we would have to avoid setting terminal phases in the API for pods that have restart policies consistent with “run forever” during graceful node shutdown.  The nodes would stop, but API status would not be reported as terminal by the Kubelet (they perhaps should be set to phase Pending).  After reboot, the pods would be restarted by the Kubelet.  As this is a behavior change, it would have to be a new optional flag to Kubelet graceful node shutdown.  

Pods that are RestartNever should get a terminal phase if the status updates propagates to the API server, or be restarted if the node shuts down without propagating the terminal phase regardless of such a flag. Pods that reach terminal phase on their own (RestartOnFailure with exit code 0\) should have a terminal phase set only if they are Succeeded with the flag set. Pods that are RestartAlways should not go to a terminal phase with the flag set.

### Readiness probes are inconsistent during pod termination

Currently there are bugs that impact readiness during pod termination that should be addressed:s

* Readiness probes stop when the internal pod phase is recorded as terminal, but that precedes when the containers are actually stopped ([bug 105780](https://github.com/kubernetes/kubernetes/issues/105780))   
  * Pods should continue to be probed until containers are stopped and not exit early  
  * Commentary: This is historic because there was no strong control over pod phase transitions in the Kubelet, now that is fixed and SyncTerminatingPod is responsible for signaling probe stop  
* Pods without readiness probes remain ready during termination  
  * We should consider reporting ready until the preStop hook exits, and then immediately transitioning the readiness to false instead of waiting for process completion

