# MC-Kube Infrastructure Setup

This repository contains the prerequisite infrastructure components for deploying **MC-Kube**, a Kubernetes operator that enforces mixed-criticality real-time (RT) scheduling policies on Linux nodes using cgroup v2 and SCHED_DEADLINE.

Before deploying the MC-Kube operator itself, the three node-level agents described below must be built and deployed to the cluster.

---

## Architecture Overview

```
┌──────────────────────────────────────────────────────────────┐
│                        Kubernetes Node                       │
│                                                              │
│   ┌─────────────────┐    ┌──────────────────────────────┐   │
│   │  cpu-util-sender │    │     ebpf-monitoring-agent    │   │
│   │   (DaemonSet)   │    │          (DaemonSet)          │   │
│   │                 │    │                               │   │
│   │ Reads node CPU  │    │  Attaches fentry probe to     │   │
│   │ utilization and │    │  update_curr_dl_se(); detects │   │
│   │ POSTs to MC-Kube│    │  SCHED_DEADLINE server        │   │
│   │ controller      │    │  budget overruns via ring     │   │
│   │                 │    │  buffer; POSTs to MC-Kube     │   │
│   └────────┬────────┘    └──────────────┬────────────────┘   │
│            │                            │                    │
│   ┌────────▼────────────────────────────▼────────────────┐   │
│   │                  MC-Kube Controller                  │   │
│   │              (Deployment, port 8090)                 │   │
│   └────────────────────────┬─────────────────────────────┘   │
│                            │ HTTP (renice / cgroup)          │
│   ┌────────────────────────▼─────────────────────────────┐   │
│   │                  node-actuator                       │   │
│   │                   (DaemonSet)                        │   │
│   │                                                      │   │
│   │  HTTP server on each node; applies cgroup v2 RT      │   │
│   │  budget (cpu.rt_period_us / cpu.rt_runtime_us) and   │   │
│   │  renice commands to containers via crictl             │   │
│   └──────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────┘
```

| Component | Image | Role |
|---|---|---|
| `cpu_util_sender` | `ghcr.io/hufs-ese-lab/cpu-util-sender:v1` | Annotates CPU utilization onto Node objects; MC-Kube reads these annotations to make scheduling decisions |
| `ebpf` | `ghcr.io/hufs-ese-lab/ebpf-monitoring-agent:v1` | Kernel-level overrun detection via eBPF `fentry` hooks on `update_curr_dl_se` |
| `resource` | `ghcr.io/hufs-ese-lab/node-actuator:v1` | Node-local HTTP actuator that applies RT cgroup parameters and renice |

---

## Prerequisites

| Requirement | Version / Notes |
|---|---|
| Linux kernel | ≥ 5.15 with BTF enabled (`CONFIG_DEBUG_INFO_BTF=y`) |
| cgroup v2 | Unified hierarchy required (`/sys/fs/cgroup` must be cgroup2) |
| containerd | ≥ 1.6 |
| crictl | Must be present at `/usr/bin/crictl` on each node |
| Docker | For building images |
| kubectl + kubeconfig | Cluster admin access |
| Go | ≥ 1.21 (local builds only) |
| clang / llvm | ≥ 14 (eBPF compilation only) |
| bpftool | For generating `vmlinux.h` (eBPF only) |
| libbpf-dev | ≥ 0.8 |

> **Kernel BTF check:**
> ```bash
> ls /sys/kernel/btf/vmlinux   # must exist
> ```

---

## Repository Structure

```
PC_setting/
├── README.md                   # This file
├── cpu_util_sender/            # CPU utilization reporter
│   ├── main.go
│   ├── Dockerfile
│   ├── go.mod
│   └── setup/
│       ├── daemonset.yaml
│       └── rbac.yaml
├── ebpf/                       # eBPF-based RT overrun monitor
│   ├── hcbs_overrun.bpf.c      # BPF program (kernel side)
│   ├── vmlinux.h               # BTF header — replace with target kernel's
│   ├── hcbs_overrun.bpf.o      # Pre-built BPF object (may need rebuild)
│   ├── hcbs_overrun.skel.h     # Generated skeleton header
│   ├── main.go                 # Userspace Go agent
│   ├── Dockerfile
│   ├── Makefile
│   └── daemonset.yaml
└── resource/                   # Node actuator (cgroup + renice)
    ├── main.go
    ├── Dockerfile
    ├── go.mod
    └── node-actuator.yaml
```

---

## Deployment

`node-actuator` and `cpu-util-sender` are bundled into the MC-Kube kustomize stack (`config/components/`) and are deployed **automatically** when you run:

```bash
# In the MC-Kube operator repository
make deploy IMG=ghcr.io/hufs-ese-lab/mc-kube:<tag>
```

`ebpf-monitoring-agent` must be deployed **separately** after building the Docker image (see `ebpf/README.md` for the required `vmlinux.h` replacement and compilation steps):

```bash
cd ebpf/
kubectl apply -f daemonset.yaml
```

Refer to each component's README for build instructions and configuration details.

---

## Related Repository

- **MC-Kube operator**: `https://github.com/hufs-ese-lab/MC-Kube`
