# Manual Verification Plan: Phase 1 -- DisruptionTarget Signals Endpoint Terminating

All scenarios use a kind cluster with a custom kube-controller-manager
binary. The goal is to verify that a disrupted pod's endpoint becomes
`Terminating=true`, `Ready=false` *before* containers stop, and that the
legacy Endpoints API moves the address to `NotReadyAddresses`.

---

## Lessons learned

- **`kind build node-image` fails with rootless Podman** due to
  `SecurityOptions` template mismatch in `build/common.sh:324` and UID
  mapping issues when mounting the source tree. Workaround: build binaries
  locally, package a custom container image, and swap it on a running
  cluster.
- **`featureGates` in kind config** applies to ALL components via kubeadm.
  If the kind base image (e.g. v1.35.0) doesn't know the gate, kubeadm
  rejects it and the kubelet won't start. Only use `featureGates` when
  building from source.
- **Static pods use container images**, not the node's `/usr/local/bin/`
  binaries. Copying a binary onto the node does nothing for apiserver/
  controller-manager/scheduler. You must either swap the container image
  or add a hostPath volume mount.
- **Binary version mismatch**: replacing the kubelet binary with a
  different minor version (e.g. v1.37-alpha kubelet on a v1.35.0 kubeadm
  config) causes the kubelet to crash (`configfiles.go` load failure).
  Only replace the kube-controller-manager via a custom container image.
- **Feature gate version**: the gate must target the repo's current
  `DefaultKubeBinaryVersion` (check `staging/src/k8s.io/component-base/
  version/base.go`). A gate targeting a future version is "PreAlpha" and
  cannot be set in tests.

---

## Prerequisites

**Step 1** -- Build the kube-controller-manager binary locally:

```bash
make WHAT="cmd/kube-controller-manager"
```

**Step 2** -- Build a container image with our custom binary:

```bash
mkdir -p /tmp/kind-kcm-build
cp _output/bin/kube-controller-manager /tmp/kind-kcm-build/

cat > /tmp/kind-kcm-build/Dockerfile << 'EOF'
FROM registry.k8s.io/kube-controller-manager:v1.35.0
COPY kube-controller-manager /usr/local/bin/kube-controller-manager
EOF

podman build -t localhost/kube-controller-manager:local /tmp/kind-kcm-build/
```

Adjust the `FROM` tag to match the kind node image version (`kind version`
will show the default node image).

**Step 3** -- Create a kind cluster (no custom feature gates in config):

```yaml
# work/kind-test-cluster.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
```

```bash
kind create cluster --name disruption-test \
  --config work/kind-test-cluster.yaml
```

**Step 4** -- Load the custom image and patch the controller manager:

```bash
kind load docker-image localhost/kube-controller-manager:local \
  --name disruption-test

# Patch the static pod manifest on the control plane node
podman exec disruption-test-control-plane bash -c '
  MANIFEST=/etc/kubernetes/manifests/kube-controller-manager.yaml
  # Replace image
  sed -i "s|image: registry.k8s.io/kube-controller-manager:.*|image: localhost/kube-controller-manager:local|" "$MANIFEST"
  # Add feature gate (append to existing --feature-gates flag)
  sed -i "s/--feature-gates=\(.*\)/--feature-gates=\1,DisruptionTargetSignalsEndpointTerminating=true/" "$MANIFEST"
'
```

Wait ~20s for the kubelet to detect the manifest change and restart the
controller manager. Verify:

```bash
kubectl get pods -n kube-system -l component=kube-controller-manager
# Should be Running, age < 30s

kubectl logs -n kube-system -l component=kube-controller-manager | head -1
# Should show our version (e.g. v1.37.0-alpha...)
```

---

## Common setup (run once per cluster)

Deploy a test workload with a long grace period so you can observe the
intermediate state:

```bash
kubectl create namespace test-disruption

kubectl -n test-disruption apply -f - <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: test-svc
spec:
  selector:
    app: sleeper
  ports:
  - port: 80
    targetPort: 8080
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sleeper
spec:
  replicas: 1
  selector:
    matchLabels:
      app: sleeper
  template:
    metadata:
      labels:
        app: sleeper
    spec:
      terminationGracePeriodSeconds: 120
      containers:
      - name: sleeper
        image: registry.k8s.io/e2e-test-images/agnhost:2.53
        args: ["serve-hostname", "--port", "8080"]
        ports:
        - containerPort: 8080
        readinessProbe:
          httpGet:
            path: /
            port: 8080
          initialDelaySeconds: 1
          periodSeconds: 2
EOF
```

Wait for the pod to be Ready:

```bash
kubectl -n test-disruption wait --for=condition=Ready pod -l app=sleeper --timeout=60s
```

Record the pod name and node for later:

```bash
POD=$(kubectl -n test-disruption get pod -l app=sleeper -o jsonpath='{.items[0].metadata.name}')
NODE=$(kubectl -n test-disruption get pod "$POD" -o jsonpath='{.spec.nodeName}')
echo "Pod: $POD  Node: $NODE"
```

Confirm the baseline state before any disruption:

```bash
# EndpointSlice: expect Ready=true, Serving=true, Terminating=false
kubectl -n test-disruption get endpointslice -l kubernetes.io/service-name=test-svc \
  -o jsonpath='{range .items[*].endpoints[*]}ready={.conditions.ready} serving={.conditions.serving} terminating={.conditions.terminating}{"\n"}{end}'

# Legacy Endpoints: expect the pod IP in .subsets[].addresses (not .notReadyAddresses)
kubectl -n test-disruption get endpoints test-svc -o yaml | grep -A5 'addresses:'
```

### Cleanup between scenarios

Deleting and re-creating the namespace is the most reliable way to clear
stale endpoints. Scaling a Deployment to 0 and back can leave ghost
terminating endpoints from disrupted pods with long grace periods:

```bash
kubectl delete namespace test-disruption --force
sleep 10
# Then re-run Common Setup
```

---

## Scenario A: Kubelet eviction (API eviction)

This simulates the eviction path that the kubelet's eviction manager uses
for resource pressure. The Eviction API sets `DisruptionTarget=True` on
the pod without setting `deletionTimestamp`.

**Step 1** -- Start a watch in a separate terminal to observe the transition:

```bash
kubectl -n test-disruption get endpointslice -l kubernetes.io/service-name=test-svc -w \
  -o custom-columns='NAME:.metadata.name,READY:.endpoints[*].conditions.ready,SERVING:.endpoints[*].conditions.serving,TERMINATING:.endpoints[*].conditions.terminating'
```

**Step 2** -- Evict the pod via the Eviction API:

```bash
# In terminal 1:
kubectl proxy --port=8001 &

# In terminal 2:
curl -X POST "http://localhost:8001/api/v1/namespaces/test-disruption/pods/$POD/eviction" \
  -H "Content-Type: application/json" \
  -d "{\"apiVersion\":\"policy/v1\",\"kind\":\"Eviction\",\"metadata\":{\"name\":\"$POD\",\"namespace\":\"test-disruption\"}}"
```

**Step 3** -- Verify (must happen within the 120s grace period):

```bash
# 3a. Pod should have DisruptionTarget=True while phase is still Running
kubectl -n test-disruption get pod "$POD" -o jsonpath='{.status.phase}'
# Expected: Running

kubectl -n test-disruption get pod "$POD" -o jsonpath='{range .status.conditions[?(@.type=="DisruptionTarget")]}{.status}{end}'
# Expected: True

# 3b. EndpointSlice should show Terminating=true, Ready=false, Serving=true
kubectl -n test-disruption get endpointslice -l kubernetes.io/service-name=test-svc \
  -o jsonpath='{range .items[*].endpoints[*]}ready={.conditions.ready} serving={.conditions.serving} terminating={.conditions.terminating}{"\n"}{end}'
# Expected: ready=false serving=true terminating=true

# 3c. Legacy Endpoints should show the pod IP in NotReadyAddresses
kubectl -n test-disruption get endpoints test-svc -o jsonpath='{.subsets[*].notReadyAddresses[*].ip}'
# Expected: the pod's IP (e.g. 10.244.1.2)

# 3d. deletionTimestamp should be set (eviction API does delete the pod)
# but verify the endpoint state changed BEFORE containers stopped:
kubectl -n test-disruption get pod "$POD" -o jsonpath='{.status.containerStatuses[0].state}'
# Expected: still shows "running" (within the grace period)
```

**Note**: Eviction also sets `deletionTimestamp`, so the endpoint is
marked terminating via both the existing path AND the new DisruptionTarget
path. This confirms the signals compose correctly. Scenario C is the one
that proves DisruptionTarget *alone* is sufficient.

---

## Scenario B: Node drain (graceful node shutdown simulation)

Draining a node cordons it and evicts all pods. This exercises the same
Eviction API path but at the node level.

**Step 1** -- Watch EndpointSlice as in Scenario A.

**Step 2** -- Drain the worker node:

```bash
kubectl drain "$NODE" --ignore-daemonsets --delete-emptydir-data --grace-period=120
```

**Step 3** -- Verify the same conditions as Scenario A, Step 3 (3a-3d)
within the 120s grace period.

**Step 4** -- Verify the pod phase is still `Running` and DisruptionTarget
is present while containers haven't stopped yet:

```bash
kubectl -n test-disruption get pod "$POD" \
  -o jsonpath='phase={.status.phase} disruption={.status.conditions[?(@.type=="DisruptionTarget")].status}'
# Expected: phase=Running disruption=True
```

**Cleanup**:

```bash
kubectl uncordon "$NODE"
# Then delete and re-create namespace (see Cleanup between scenarios)
```

---

## Scenario C: Direct DisruptionTarget patch (synthetic)

**This is the fastest and most reliable test.** It exercises only the
controller path (no kubelet changes needed), takes seconds, and isolates
the new behavior. Run it first.

This directly patches DisruptionTarget onto a running pod, simulating what
the kubelet does internally.

**Step 1** -- Watch EndpointSlice as in Scenario A.

**Step 2** -- Patch the pod's status to add DisruptionTarget:

```bash
kubectl -n test-disruption patch pod "$POD" --subresource=status --type=merge -p '{
  "status": {
    "conditions": [{
      "type": "DisruptionTarget",
      "status": "True",
      "reason": "ManualTest",
      "lastTransitionTime": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'"
    }]
  }
}'
```

**Step 3** -- Verify:

```bash
# 3a. Pod phase should still be Running (no deletion, no eviction)
kubectl -n test-disruption get pod "$POD" -o jsonpath='{.status.phase}'
# Expected: Running

# 3b. deletionTimestamp should be nil (we only patched status)
kubectl -n test-disruption get pod "$POD" -o jsonpath='{.metadata.deletionTimestamp}'
# Expected: (empty)

# 3c. EndpointSlice: Terminating=true, Ready=false, Serving=true
kubectl -n test-disruption get endpointslice -l kubernetes.io/service-name=test-svc \
  -o jsonpath='{range .items[*].endpoints[*]}ready={.conditions.ready} serving={.conditions.serving} terminating={.conditions.terminating}{"\n"}{end}'
# Expected: ready=false serving=true terminating=true

# 3d. Legacy Endpoints: pod IP in NotReadyAddresses
kubectl -n test-disruption get endpoints test-svc -o jsonpath='{.subsets[*].notReadyAddresses[*].ip}'
# Expected: the pod's IP
```

**Cleanup**: Delete and re-create namespace (see Cleanup between scenarios).

---

## Scenario D: Feature gate disabled (negative test)

Verify that with the gate disabled, DisruptionTarget does NOT affect
endpoints.

A separate cluster is NOT needed. The stock kind cluster (without the
custom kube-controller-manager) already has the gate off -- the v1.35.0
controller-manager doesn't know the gate. If you want to test this
explicitly on the same cluster, temporarily swap the KCM back to the
stock image:

```bash
podman exec disruption-test-control-plane bash -c '
  sed -i "s|image: localhost/kube-controller-manager:local|image: registry.k8s.io/kube-controller-manager:v1.35.0|" \
    /etc/kubernetes/manifests/kube-controller-manager.yaml
  sed -i "s/,DisruptionTargetSignalsEndpointTerminating=true//" \
    /etc/kubernetes/manifests/kube-controller-manager.yaml
'
# Wait ~20s for restart
```

**Step 1** -- Deploy the workload and wait for Ready (same as Common Setup).

**Step 2** -- Patch DisruptionTarget onto the pod (same as Scenario C,
Step 2).

**Step 3** -- Verify the endpoint is NOT affected:

```bash
# EndpointSlice: should still show Ready=true, Terminating=false
kubectl -n test-disruption get endpointslice -l kubernetes.io/service-name=test-svc \
  -o jsonpath='{range .items[*].endpoints[*]}ready={.conditions.ready} serving={.conditions.serving} terminating={.conditions.terminating}{"\n"}{end}'
# Expected: ready=true serving=true terminating=false

# Legacy Endpoints: pod IP should still be in Addresses (not NotReadyAddresses)
kubectl -n test-disruption get endpoints test-svc -o jsonpath='{.subsets[*].addresses[*].ip}'
# Expected: the pod's IP
```

**Cleanup**: Swap the KCM image back to the custom one if continuing
with other scenarios.

---

## Scenario E: Job controller is not affected

Verify that early DisruptionTarget does not cause the job controller to
prematurely trigger pod failure policy retries.

**Step 1** -- Deploy a Job with a pod failure policy:

```bash
kubectl -n test-disruption apply -f - <<'EOF'
apiVersion: batch/v1
kind: Job
metadata:
  name: test-job
spec:
  backoffLimit: 3
  podFailurePolicy:
    rules:
    - action: Count
      onPodConditions:
      - type: DisruptionTarget
  template:
    metadata:
      labels:
        job-name: test-job
    spec:
      terminationGracePeriodSeconds: 120
      restartPolicy: Never
      containers:
      - name: worker
        image: registry.k8s.io/e2e-test-images/agnhost:2.53
        args: ["pause"]
EOF
```

Wait for the Job pod to be Running:

```bash
kubectl -n test-disruption wait --for=condition=Ready pod -l job-name=test-job --timeout=60s
JOB_POD=$(kubectl -n test-disruption get pod -l job-name=test-job -o jsonpath='{.items[0].metadata.name}')
```

**Step 2** -- Patch DisruptionTarget onto the running Job pod:

```bash
kubectl -n test-disruption patch pod "$JOB_POD" --subresource=status --type=merge -p '{
  "status": {
    "conditions": [{
      "type": "DisruptionTarget",
      "status": "True",
      "reason": "ManualTest",
      "lastTransitionTime": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'"
    }]
  }
}'
```

**Step 3** -- Verify the Job does NOT count this as a failure (pod is
still Running, not terminal):

```bash
# Pod phase should still be Running
kubectl -n test-disruption get pod "$JOB_POD" -o jsonpath='{.status.phase}'
# Expected: Running

# Job should show 0 failed pods (the failure policy only triggers on terminal pods)
kubectl -n test-disruption get job test-job -o jsonpath='{.status.failed}'
# Expected: (empty or 0)

# Job should not have created a replacement pod
kubectl -n test-disruption get pods -l job-name=test-job --no-headers | wc -l
# Expected: 1
```

**Cleanup**:

```bash
kubectl -n test-disruption delete job test-job
```

---

## Cluster teardown

```bash
kind delete cluster --name disruption-test
kubectl config use-context <your-previous-context>
```

---

## Practical tips

- **Scenario C is the fastest and most reliable test.** It exercises only
  the controller path (no kubelet changes needed), takes seconds, and
  isolates the new behavior. Run it first.
- **Cleanup between scenarios**: deleting and re-creating the namespace is
  the most reliable way to clear stale endpoints. Scaling a Deployment to
  0 and back can leave ghost terminating endpoints from disrupted pods
  with long grace periods.
- **Eviction scenarios (A, B) also set `deletionTimestamp`**, so the
  endpoint is marked terminating via both the existing path AND the new
  DisruptionTarget path. These scenarios confirm the signals compose
  correctly, but Scenario C is the one that proves DisruptionTarget alone
  is sufficient.
- **Podman/kind provider**: kind auto-detects Podman via the `docker`
  symlink. If it prints "enabling experimental podman provider", things
  are working. Setting `KIND_EXPERIMENTAL_PROVIDER=podman` is optional.
- **Stale pods after scale-down**: if you see multiple endpoints (some
  terminating) after a scale-down, wait for the grace period to expire or
  `kubectl delete pod <name> --grace-period=0 --force`.
