# gpuhealth

A small Kubernetes operator that cordons and taints nodes when a (simulated) GPU health check fails, and restores them when the node is healthy again.

> **Status: learning project.** I built this to get hands-on with Kubernetes controller development (controller-runtime, CRDs, reconcile loops, RBAC). The health signal is simulated by hand-written custom resources, and it has only been run against a local [kind](https://kind.sigs.k8s.io/) cluster. It is not production software.

## What it does

You declare the health of a node by creating a `GPUHealthReport` custom resource. The controller compares that declared intent with the node's actual state and corrects any difference.

```
 GPUHealthReport (spec: nodeName, healthy)        <- desired state, written by a person or a detector
          |
          v
   gpuhealth controller  --watches-->  GPUHealthReport and Node objects
          |
          +-- healthy: false -> cordon node + add taint example.com/gpu-unhealthy:NoSchedule
          +-- healthy: true  -> remove taint + uncordon
          |
          v
 report.status (remediated, message)               <- observed state, written by the controller
```

Cordoning and tainting stop **new** pods from being scheduled onto the node. Pods already running are left alone.

## Behaviors

| Feature | What it does |
|---|---|
| Remediation | Unhealthy report cordons and taints the node. Healthy report reverses it. |
| Idempotent writes | The controller patches a node or updates status only when something differs. |
| Drift correction | The controller also watches Nodes. If someone removes the taint or uncordons a node that a report says is unhealthy, the controller reapplies them. |
| Remediation budget | At most about a third of nodes (minimum 1) may be remediated at once. Further remediations are blocked, reported in `status.message`, and retried every 30 seconds. |
| Human override | Annotate a node with `example.com/gpu-health-paused: "true"` and the controller takes no action on it, and says so in the report's status. |

## Design decisions

- **Level-triggered and idempotent.** Each reconcile reads current state and converges toward the declared spec, so a crash, a restart, or duplicate events do no harm. The controller never depends on having seen a particular event.
- **Spec is intent, status is observation.** The report's `spec` is written by whoever reports health. Only the controller writes `status`.
- **Watching Nodes is a trigger, not a source of truth.** A node change wakes the controller, but the report still defines what the node should look like. Manual edits to managed fields are treated as drift.
- **Blast-radius control.** A faulty detector must not be able to cordon the whole fleet. The budget turns a bad signal into a visible, blocked state instead of an outage.
- **Explicit human override.** Automation that always wins fights the engineer who deliberately takes a node out for repair. The pause annotation makes that override declared and visible instead of a silent fight.
- **Cordon plus a custom taint.** Cordon is the generic "do not schedule" switch. The taint records the reason and lets specific pods (for example diagnostics) tolerate it.

## Prerequisites

- Go (1.22 or newer), Docker (or Colima), `kubectl`, [`kind`](https://kind.sigs.k8s.io/)
- [`kubebuilder`](https://book.kubebuilder.io/) only if you want to regenerate code

## Quick start

```bash
# 1. Create a local cluster with two worker nodes
kind create cluster --name gpu-practice --config hack/kind.yaml
kubectl get nodes

# 2. Install the CRD
make install

# 3. Run the controller against the cluster (leave this running)
make run
```

In a second terminal:

```bash
# Mark a worker unhealthy
kubectl apply -f examples/report-unhealthy.yaml
kubectl get nodes                                   # worker shows SchedulingDisabled
kubectl describe node gpu-practice-worker | grep -iE "taints|unschedulable"
kubectl get gpuhealthreport worker1-report -o yaml  # status.remediated: true

# Mark it healthy again
kubectl apply -f examples/report-healthy.yaml
kubectl get nodes
```

Clean up with `kind delete cluster --name gpu-practice`.

## Try the behaviors

```bash
# Drift correction: undo the remediation by hand (the controller reapplies it)
kubectl apply -f examples/report-unhealthy.yaml
kubectl uncordon gpu-practice-worker
kubectl taint nodes gpu-practice-worker example.com/gpu-unhealthy-

# Human override: the controller leaves the node alone while paused
kubectl annotate node gpu-practice-worker example.com/gpu-health-paused=true
kubectl uncordon gpu-practice-worker
kubectl taint nodes gpu-practice-worker example.com/gpu-unhealthy-
kubectl annotate node gpu-practice-worker example.com/gpu-health-paused-   # unpause

# Budget: with two unhealthy reports on a 3-node cluster, the second is blocked
# (create a second report for gpu-practice-worker2 with healthy: false and
# check status.message)
```

## Project layout

| Path | Purpose |
|---|---|
| `api/v1alpha1/gpuhealthreport_types.go` | The `GPUHealthReport` API (spec and status) |
| `internal/controller/gpuhealthreport_controller.go` | Reconcile logic, budget, pause handling, Node watch |
| `cmd/main.go` | Manager entry point (generated) |
| `config/` | Generated CRD, RBAC, and deployment manifests |
| `examples/`, `hack/` | Sample reports and the kind cluster config |

## Known limitations and next steps

- Deleting a report does not undo its effect (a finalizer would fix this).
- The budget reads from the informer cache, which can lag recent writes, so two near-simultaneous remediations could both pass. An uncached read or a lease would close that gap.
- The budget counts all nodes, including the control plane, and has no notion of node pools or GPU types.
- No drain: existing pods are never evicted. A real version would evict safely, respecting PodDisruptionBudgets.
- No flap damping, events, or Prometheus metrics, and no automated tests yet (envtest would be the natural start).
- A real deployment would feed reports from a node-level exporter such as DCGM or node-problem-detector instead of hand-written resources.

## Notes for macOS

If `make` fails with linker errors on a mismatched Xcode toolchain, building without cgo avoids the system linker:

```bash
export CGO_ENABLED=0
echo 'export CGO_ENABLED=0' >> ~/.bash_profile   #make it permanent
```