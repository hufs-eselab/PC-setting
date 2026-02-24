# Node Actuator (`node-actuator`)

## Overview

`node-actuator` is a privileged DaemonSet that runs on every Kubernetes node and exposes a local HTTP API for applying real-time (RT) scheduling parameters to containers. The MC-Kube controller calls this agent whenever it needs to:

- Adjust a container's SCHED_DEADLINE budget (`cpu.rt_period_us` / `cpu.rt_runtime_us`) inside cgroup v2.
- Pin a container to a specific CPU core (`cpuset.cpus`).
- Change a container's nice value (`renice`) via `crictl` and the kernel `/proc`.

This agent acts as the **actuator layer** in the MC-Kube mixed-criticality system: the controller makes scheduling decisions, and the node-actuator enforces them at the OS level.

---

## Architecture

```
MC-Kube Controller
  │
  │  HTTP POST /cgroup  { container_id, period, runtime, core, only_runtime }
  │  HTTP POST /renice  { container_id, nice }
  ▼
node-actuator Pod (hostNetwork, privileged)
  ├── /cgroup handler
  │     ├── crictl inspect <container_id>  →  cgroup path
  │     ├── Write cpu.rt_period_us  (container + pod cgroup)
  │     ├── Write cpu.rt_runtime_us (container + pod cgroup)
  │     ├── Write cpuset.cpus       (if core != nil)
  │     └── Write cpu.rt_multi_runtime_us (per-core budget)
  └── /renice handler
        ├── crictl inspect <container_id>  →  PID
        └── renice -n <nice> -p <PID>
```

### cgroup Write Order

To avoid temporarily exceeding parent limits, the agent applies cgroup values in the correct order:

- **Decreasing limits** (tightening): container cgroup first, then pod cgroup.
- **Increasing limits** (relaxing): pod cgroup first, then container cgroup.

This prevents the kernel from rejecting writes due to parent/child budget inconsistency.

---

## HTTP API

The agent listens on **TCP `0.0.0.0:8080`** on the host network.

### `POST /renice`

Change the nice value of a container's main process.

**Request body:**
```json
{
  "container_id": "containerd://<full-id>",
  "nice": -10
}
```

**Flow:** `crictl inspect` → extract PID → `renice -n <nice> -p <pid>`

---

### `POST /cgroup`

Set RT cgroup parameters for a container.

**Request body:**
```json
{
  "container_id": "containerd://<full-id>",
  "period":      1000000,
  "runtime":     500000,
  "core":        "2",
  "only_runtime": false
}
```

| Field | Type | Description |
|---|---|---|
| `container_id` | string | Full container ID (with or without `containerd://` prefix) |
| `period` | int (μs) | RT period written to `cpu.rt_period_us` |
| `runtime` | int (μs) | RT runtime written to `cpu.rt_runtime_us` |
| `core` | string (optional) | CPU core ID to pin via `cpuset.cpus` and `cpu.rt_multi_runtime_us` |
| `only_runtime` | bool | If `true`, skip period update (escalation mode: only tighten runtime) |

**Cgroup files written:**

| File | Scope |
|---|---|
| `cpu.rt_period_us` | container cgroup + pod cgroup |
| `cpu.rt_runtime_us` | container cgroup + pod cgroup |
| `cpu.rt_multi_runtime_us` | container cgroup + pod cgroup (if `core` set) |
| `cpuset.cpus` | container cgroup + pod cgroup (if `core` set) |

> **Note:** The system-level `sched_rt_runtime_us` must not be `-1` (disabled). If RT throttling is globally disabled, the agent returns an error.

---

### `GET /healthz`

Returns HTTP 200. Used by the Kubernetes liveness probe.

---

## Prerequisites

- cgroup v2 unified hierarchy (`/sys/fs/cgroup` must be mounted as `cgroup2`)
- `crictl` available at `/usr/bin/crictl` on the host
- `jq` installed in the container (included in the Docker image)
- containerd socket accessible at `/run/containerd/containerd.sock`
- Privileged security context (required for writing to cgroup files)

---

## Build

```bash
cd resource/

docker build -t ghcr.io/hufs-ese-lab/node-actuator:v1 .
docker push ghcr.io/hufs-ese-lab/node-actuator:v1
```

---

## Deploy

> **This component is deployed automatically as part of the MC-Kube kustomize stack.**
> Running `make deploy` in the MC-Kube operator repository deploys this DaemonSet via `config/components/node-actuator.yaml`. You do **not** need to apply the manifest in this directory manually.

To deploy standalone (outside of MC-Kube kustomize), or for development/testing:

```bash
cd resource/
kubectl apply -f node-actuator.yaml

# Verify
kubectl get ds -n mc-kube-system mc-kube-node-actuator
kubectl logs -n mc-kube-system -l name=node-actuator --tail=20
```

---

## Verify Endpoint (from within the cluster)

```bash
# From any pod in mc-kube-system namespace, or using hostNetwork:
curl -s http://<node-ip>:8080/healthz
# Expected: {"status":"ok"}

# Test renice (example)
curl -s -X POST http://<node-ip>:8080/renice \
  -H 'Content-Type: application/json' \
  -d '{"container_id":"containerd://<id>","nice":-5}'
```

---

## Directory Structure

```
resource/
├── main.go              # HTTP server: /renice, /cgroup, /healthz handlers
├── main_test.go         # Unit tests for cgroup path resolution
├── Dockerfile           # alpine-based image with jq
├── go.mod
├── node-actuator.yaml   # DaemonSet + ServiceAccount + RBAC
└── install-daemon.sh    # Manual install helper (non-Kubernetes environments)
```
