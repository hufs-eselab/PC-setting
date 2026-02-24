# eBPF Monitoring Agent (`ebpf-monitoring-agent`)

## Overview

`ebpf-monitoring-agent` is a kernel-level RT budget overrun detector built with **eBPF CO-RE** (Compile Once – Run Everywhere). It attaches an `fentry` probe to the kernel function `update_curr_dl_se()` to monitor SCHED_DEADLINE server entities in real time. When a deadline server's runtime budget goes negative (overrun) while RT tasks are actually running, the agent reports the event to the MC-Kube controller, which in turn triggers preemption of lower-criticality RT tasks.

### Key Behavior

- Attaches `fentry/update_curr_dl_se` to intercept every scheduling tick of each SCHED_DEADLINE server.
- Filters out:
  - Non-server entities (`dl_server == 0`)
  - Overruns with no active RT tasks (`rt_nr_running == 0`, i.e., idle cgroup replenishment)
  - Hard-coded system-level servers (`dl_runtime == 50 ms`)
- Fires only when `runtime ≤ −100 μs` **and** `dl_throttled == 1` (threshold: `OVERRUN_THRESHOLD_NS = -100,000`).
- Sends events through a **16 MB ring buffer** to the userspace Go agent.
- Userspace agent resolves the event's cgroup ID to a Kubernetes pod, then HTTP POSTs to:

```
POST http://mc-kube-controller.mc-kube-system.svc.cluster.local:8090/overrun
{
  "node": "<node-name>",
  "pod": "<pod-name>",
  "namespace": "<namespace>",
  "runtime_ns": <negative-value>,
  "cpu": <cpu-id>
}
```

---

## Architecture

```
Kernel (SCHED_DEADLINE tick)
 └── fentry/update_curr_dl_se()
       ├── dl_server? && nr_running > 0?
       ├── runtime ≤ -100,000 ns && throttled?
       └── ringbuf_submit(event{ts, cpu, nr_running, runtime_ns, cgid})
                │
                ▼ (cilium/ebpf ringbuf reader)
    Userspace Go Agent (hcbs-agent)
       ├── cgid → /sys/fs/cgroup path → pod name
       ├── HTTP POST /overrun  →  MC-Kube Controller
       └── Exported metrics (future)
```

---

## Prerequisites

| Requirement | Version / Notes |
|---|---|
| Linux kernel | ≥ 5.15 with `CONFIG_DEBUG_INFO_BTF=y` |
| clang / llvm | ≥ 14 |
| libbpf-dev | ≥ 0.8 |
| bpftool | For generating `vmlinux.h` |
| Docker | For building the container image |
| cgroup v2 | `/sys/fs/cgroup` must be cgroup2 unified hierarchy |

---

## ⚠️ Important: Replacing `vmlinux.h` for Your Kernel

`hcbs_overrun.bpf.c` uses **BTF-based CO-RE** via `vmlinux.h`. The `vmlinux.h` in this repository was generated from a specific kernel version. **You must replace it with one generated from your target node's running kernel** before compiling.

### Step 1 — Generate `vmlinux.h` from the target kernel

On the target node (or a machine running the same kernel version):

```bash
# Verify BTF is available
ls /sys/kernel/btf/vmlinux

# Generate vmlinux.h
bpftool btf dump file /sys/kernel/btf/vmlinux format c > vmlinux.h
```

Copy the generated `vmlinux.h` into this directory, replacing the existing file:

```bash
cp vmlinux.h /path/to/PC_setting/ebpf/vmlinux.h
```

### Step 2 — Compile the eBPF object

```bash
cd ebpf/

clang -O2 -g \
  -target bpf \
  -D__TARGET_ARCH_x86 \
  -c hcbs_overrun.bpf.c \
  -o hcbs_overrun.bpf.o
```

> On non-x86 architectures replace `-D__TARGET_ARCH_x86` with the appropriate
> value (e.g., `-D__TARGET_ARCH_arm64`).

### Step 3 — (Optional) Regenerate the skeleton header

The skeleton header `hcbs_overrun.skel.h` is used only if you switch to a skeleton-based loader. The current Go agent uses the `cilium/ebpf` library directly (no skeleton needed), but you can regenerate it for reference:

```bash
bpftool gen skeleton hcbs_overrun.bpf.o > hcbs_overrun.skel.h
```

---

## Build Docker Image

The Dockerfile expects `hcbs_overrun.bpf.o` to be **pre-built** and present in the directory before the Docker build (it is copied in as a pre-built artifact):

```bash
cd ebpf/

# 1. Compile BPF object (see above)
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 \
  -c hcbs_overrun.bpf.c -o hcbs_overrun.bpf.o

# 2. Build Docker image
make build
# or directly:
docker build -t ghcr.io/hufs-ese-lab/ebpf-monitoring-agent:v1 .

# 3. Push to registry
make push
# or directly:
docker push ghcr.io/hufs-ese-lab/ebpf-monitoring-agent:v1
```

---

## Deploy

After building and pushing the Docker image, deploy the DaemonSet manually:

```bash
cd ebpf/
kubectl apply -f daemonset.yaml

# Check status
make status

# Tail logs
make logs
```

> **Note:** Unlike `node-actuator` and `cpu-util-sender`, this component is **not** bundled into the MC-Kube kustomize stack. It must be deployed separately before or after the MC-Kube operator.

---

## Makefile Targets

| Target | Description |
|---|---|
| `make bpf` | Compile `hcbs_overrun.bpf.c` → `hcbs_overrun.bpf.o` |
| `make build` | Build Docker image |
| `make push` | Push Docker image to GHCR |
| `make deploy` | `kubectl apply -f daemonset.yaml` |
| `make status` | Show DaemonSet and pod status |
| `make logs` | Tail pod logs |
| `make undeploy` | `kubectl delete -f daemonset.yaml` |
| `make clean` | Remove local build artifacts |

---

## Event Schema

The BPF ring buffer emits the following struct per overrun event:

```c
struct event {
    __u64 ts;          // ktime_get_ns() timestamp
    __u32 cpu;         // CPU ID where overrun occurred
    __u32 nr_running;  // Number of RT tasks in cgroup at overrun
    __s64 runtime_ns;  // Remaining budget (negative = overrun magnitude)
    __u32 _pad[2];
    __u64 tg_ptr;      // Pointer to task_group (kernel)
    __u64 cgid;        // cgroup ID → resolved to pod name in userspace
};
```

---

## Directory Structure

```
ebpf/
├── hcbs_overrun.bpf.c      # BPF program (kernel-side, fentry hook)
├── vmlinux.h               # BTF all-in-one header — REPLACE FOR YOUR KERNEL
├── hcbs_overrun.bpf.o      # Pre-built BPF object (rebuild after vmlinux.h change)
├── hcbs_overrun.skel.h     # BPF skeleton header (informational)
├── main.go                 # Userspace agent: ring buffer reader + HTTP reporter
├── go.mod
├── Dockerfile              # ubuntu:22.04 runtime with pre-built BPF object
├── Makefile
├── daemonset.yaml          # DaemonSet (privileged, hostPID, hostNetwork)
├── svc.yaml                # (optional) ClusterIP service for metrics
├── svc_node.yaml           # (optional) NodePort service
└── analyze_overrun.sh      # Offline log analysis helper
```
