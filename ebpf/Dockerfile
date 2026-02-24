# Stage 1: Build eBPF object and Go binary
FROM ubuntu:22.04 AS builder

# Install dependencies
RUN apt-get update && apt-get install -y \
    clang \
    llvm \
    libbpf-dev \
    linux-headers-generic \
    golang-1.21 \
    make \
    git \
    && rm -rf /var/lib/apt/lists/*

# Setup Go environment
ENV PATH="/usr/lib/go-1.21/bin:${PATH}"
ENV GOPATH=/go
ENV PATH="${GOPATH}/bin:${PATH}"

WORKDIR /build

# Copy source files
COPY hcbs_overrun.bpf.c vmlinux.h go.mod go.sum main.go ./

# Build eBPF object (already compiled, but keeping for reference)
# RUN clang -O2 -g -target bpf -D__TARGET_ARCH_x86 -c hcbs_overrun.bpf.c -o hcbs_overrun.bpf.o

# Copy pre-built eBPF object
COPY hcbs_overrun.bpf.o .

# Download Go dependencies
RUN go mod download

# Build Go binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o hcbs-agent main.go

# Stage 2: Runtime image
FROM ubuntu:22.04

# Install minimal runtime dependencies
RUN apt-get update && apt-get install -y \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy binaries from builder
COPY --from=builder /build/hcbs-agent /app/
COPY --from=builder /build/hcbs_overrun.bpf.o /app/

# Set environment variables
ENV BPF_OBJECT=/app/hcbs_overrun.bpf.o

# Run as root (required for eBPF)
USER root

ENTRYPOINT ["/app/hcbs-agent"]
