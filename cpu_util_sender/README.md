# CPU Utilization Sender (`cpu-util-sender`)

## Overview

`cpu-util-sender` is a DaemonSet-based Go agent that continuously monitors host CPU utilization on each Kubernetes node and annotates the Node object with the measured values. The MC-Kube controller watches these annotations to make admission and preemption decisions — preventing new real-time (RT) pods from being admitted when the node is already heavily loaded.

### Key Behavior

- Samples `/proc/stat` every **1 second** to compute aggregate CPU utilization.
- Tracks cumulative time the node has spent above the **90 % utilization threshold**.
- Patches the Node object via `kubectl annotate --overwrite`:

| Annotation | Type | Description |
|---|---|---|
| `mckube.sdv.com/cpu-usage` | integer (%) | Current CPU utilization |
| `mckube.sdv.com/cpu-over90-duration-s` | integer (s) | Seconds the node has continuously exceeded 90 % |

- Also exposes a lightweight HTTP API on port `8080` for in-cluster annotation updates:
  - `POST/PATCH /api/v1/nodes/{nodeName}/annotations`
  - `GET /health`

---

## Architecture

```
Node
 └── cpu-util-sender Pod
       ├── /proc/stat  ──poll 1s──►  compute usage %
       ├── usage > 90% → increment over90-duration counter
       ├── kubectl annotate node <nodeName> --overwrite
       │       mckube.sdv.com/cpu-usage=<N>
       │       mckube.sdv.com/cpu-over90-duration-s=<T>
       └── HTTP :8080
             └── PATCH /api/v1/nodes/{nodeName}/annotations
```

The MC-Kube controller reads these annotations on Node watch events to update its internal CPU pool state and apply the `coreUtilizationThreshold` (default 90 %) guard before admitting new RT tasks.

---

## Prerequisites

- Kubernetes cluster with RBAC enabled
- ServiceAccount with `get`, `list`, `patch` on `nodes` resource (provided in `setup/rbac.yaml`)
- Node name injected via the downward API as the `NODE_NAME` environment variable

---

## Build

```bash
cd cpu_util_sender/

docker build -t ghcr.io/hufs-ese-lab/cpu-util-sender:v1 .
docker push ghcr.io/hufs-ese-lab/cpu-util-sender:v1
```

---

## Deploy

> **This component is deployed automatically as part of the MC-Kube kustomize stack.**
> Running `make deploy` in the MC-Kube operator repository deploys this DaemonSet via `config/components/cpu-monitoring-agent.yaml`. You do **not** need to apply the manifests in this directory manually.

To deploy standalone (outside of MC-Kube kustomize), or for development/testing:

```bash
cd cpu_util_sender/

kubectl apply -f setup/rbac.yaml
kubectl apply -f setup/daemonset.yaml

# Verify
kubectl get ds -n mc-kube-system
kubectl logs -n mc-kube-system -l app=mc-kube-cpu-agent --tail=20
```

---

## Verify Node Annotations

```bash
kubectl get node <node-name> -o jsonpath='{.metadata.annotations}' \
  | python3 -m json.tool | grep mckube
```

Expected:
```
"mckube.sdv.com/cpu-usage": "12",
"mckube.sdv.com/cpu-over90-duration-s": "0"
```

---

## Directory Structure

```
cpu_util_sender/
├── main.go              # Agent: /proc/stat sampling + kubectl annotate
├── Dockerfile           # Single-stage Go build
├── go.mod
└── setup/
    ├── daemonset.yaml   # DaemonSet manifest (mc-kube-system namespace)
    └── rbac.yaml        # ClusterRole + ClusterRoleBinding
```
