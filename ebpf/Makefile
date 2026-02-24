# Makefile for eBPF Monitoring Agent

IMAGE_NAME ?= ghcr.io/hufs-ese-lab/ebpf-monitoring-agent
IMAGE_TAG ?= v1

.PHONY: all build push deploy clean

all: build push

# Build eBPF object (if needed)
bpf:
	clang -O2 -g -target bpf -D__TARGET_ARCH_x86 -c hcbs_overrun.bpf.c -o hcbs_overrun.bpf.o
	@echo "eBPF object built: hcbs_overrun.bpf.o"

# Build Docker image
build:
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .
	@echo "Docker image built: $(IMAGE_NAME):$(IMAGE_TAG)"

# Push Docker image
push:
	docker push $(IMAGE_NAME):$(IMAGE_TAG)
	@echo "Docker image pushed: $(IMAGE_NAME):$(IMAGE_TAG)"

# Build and push in one command
docker-build-push: build push

# Deploy to Kubernetes
deploy:
	kubectl apply -f daemonset.yaml
	@echo "DaemonSet deployed to mc-kube-system namespace"

# Check deployment status
status:
	kubectl get daemonset -n mc-kube-system ebpf-monitoring-agent
	kubectl get pods -n mc-kube-system -l app=ebpf-monitoring-agent

# View logs
logs:
	kubectl logs -n mc-kube-system -l app=ebpf-monitoring-agent --tail=100 -f

# Delete deployment
undeploy:
	kubectl delete -f daemonset.yaml
	@echo "DaemonSet removed from cluster"

# Clean local artifacts
clean:
	rm -f hcbs_overrun.bpf.o hcbs-agent
	@echo "Cleaned local build artifacts"

# Full cleanup (including Docker image)
clean-all: clean
	docker rmi $(IMAGE_NAME):$(IMAGE_TAG) || true
	@echo "Cleaned all artifacts including Docker image"
