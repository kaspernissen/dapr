# Build Instructions for Dapr with Workflow Activity Tracing

## Overview

This document provides step-by-step instructions for building Dapr with workflow activity tracing improvements.

## Prerequisites

- Go 1.21 or later
- Docker
- Make
- Access to container registry (Docker Hub or similar)

## Build Steps

### 1. Navigate to the Repository

```bash
cd /Users/kaspernissen/dash0/dapr/newnewclone/dapr
```

### 2. Verify Changes

Check that the workflow tracing changes are present:

```bash
git status
```

Expected modified files:
- `pkg/actors/targets/workflow/activity/execute.go`
- `pkg/actors/targets/workflow/orchestrator/activity.go`
- `go.mod` (replace directive for durabletask-go)
- `go.sum`

### 3. Ensure durabletask-go is Built

The `go.mod` should have a replace directive pointing to your local durabletask-go:

```bash
grep "durabletask-go" go.mod
```

Expected output:
```
replace github.com/dapr/durabletask-go => /Users/kaspernissen/dash0/dapr/newnewclone/durabletask-go
```

Make sure durabletask-go is built (see durabletask-go BUILD.md):
```bash
cd /Users/kaspernissen/dash0/dapr/newnewclone/durabletask-go
go build ./...
cd /Users/kaspernissen/dash0/dapr/newnewclone/dapr
```

### 4. Set Build Environment Variables

```bash
# Set your container registry
export DAPR_REGISTRY=kaspernissen

# Set the tag for your build
export DAPR_TAG=tracing-v9

# Set target platform
export TARGET_OS=linux
export TARGET_ARCH=arm64
```

Note: Change `DAPR_REGISTRY` to your Docker Hub username or container registry.

### 5. Build Dapr Binaries

```bash
# Build all binaries
make build
```

This builds:
- `daprd` - Dapr sidecar
- `placement` - Placement service
- `operator` - Dapr operator
- `injector` - Sidecar injector
- `sentry` - Certificate authority

Expected output:
```
Building daprd...
Building placement...
Building operator...
Building injector...
Building sentry...
```

Binaries are created in `dist/{os}_{arch}/release/` directory.

### 6. Build Docker Images

```bash
# Build Docker images for specified architecture
make docker-build
```

This creates:
- `kaspernissen/daprd:tracing-v9-linux-arm64`
- `kaspernissen/placement:tracing-v9-linux-arm64`
- `kaspernissen/operator:tracing-v9-linux-arm64`
- `kaspernissen/injector:tracing-v9-linux-arm64`
- `kaspernissen/sentry:tracing-v9-linux-arm64`

Expected output:
```
Building docker image for daprd...
Successfully tagged kaspernissen/daprd:tracing-v9-linux-arm64
```

### 7. Push Docker Images

```bash
# Login to Docker Hub (if not already logged in)
docker login

# Push images
make docker-push
```

Or push specific images:
```bash
docker push kaspernissen/daprd:tracing-v9-linux-arm64
```

## Quick Build Script

For convenience, use this one-liner to build and push:

```bash
cd /Users/kaspernissen/dash0/dapr/newnewclone/dapr && \
export DAPR_REGISTRY=kaspernissen && \
export DAPR_TAG=tracing-v9 && \
export TARGET_OS=linux && \
export TARGET_ARCH=arm64 && \
make docker-build && \
docker push kaspernissen/daprd:tracing-v9-linux-arm64
```

## Building for Multiple Architectures

### AMD64 (x86_64)

```bash
export TARGET_OS=linux
export TARGET_ARCH=amd64
make docker-build
docker push kaspernissen/daprd:tracing-v9-linux-amd64
```

### ARM64 (Apple Silicon, ARM servers)

```bash
export TARGET_OS=linux
export TARGET_ARCH=arm64
make docker-build
docker push kaspernissen/daprd:tracing-v9-linux-arm64
```

### Multi-Architecture Image (Manifest)

To create a single image tag that supports multiple architectures:

```bash
# Build both architectures
export DAPR_TAG=tracing-v9
export TARGET_OS=linux

export TARGET_ARCH=amd64
make docker-build
docker push kaspernissen/daprd:tracing-v9-linux-amd64

export TARGET_ARCH=arm64
make docker-build
docker push kaspernissen/daprd:tracing-v9-linux-arm64

# Create and push manifest
docker manifest create kaspernissen/daprd:tracing-v9 \
  kaspernissen/daprd:tracing-v9-linux-amd64 \
  kaspernissen/daprd:tracing-v9-linux-arm64

docker manifest push kaspernissen/daprd:tracing-v9
```

## Changes Summary

The build includes:

1. **Workflow Orchestrator** (`pkg/actors/targets/workflow/orchestrator/activity.go`):
   - Changed from span links to trace context continuation
   - Producer spans now have workflow span as parent

2. **Activity Execution** (`pkg/actors/targets/workflow/activity/execute.go`):
   - Consumer spans now continue trace from producer spans
   - Added logging for trace context propagation

3. **Dependencies** (`go.mod`):
   - Uses local durabletask-go with trace context extraction

## Deployment

### Kubernetes

Update your deployment YAML files to use the new image:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pizza-store-deployment
spec:
  template:
    metadata:
      annotations:
        dapr.io/enabled: "true"
        dapr.io/app-id: "pizza-store"
        dapr.io/sidecar-image: "kaspernissen/daprd:tracing-v9-linux-arm64-linux-arm64"
```

Apply the changes:
```bash
kubectl apply -f deployment.yaml
```

### Helm

If using Helm to deploy Dapr:

```bash
helm upgrade dapr dapr/dapr \
  --set global.registry=kaspernissen \
  --set global.tag=tracing-v9-linux-arm64
```

## Verification

### Check Container Image

```bash
docker images | grep daprd
```

Expected output:
```
kaspernissen/daprd   tracing-v9-linux-arm64   abc123def456   2 minutes ago   200MB
```

### Verify Binary Version

```bash
./dist/linux_arm64/release/daprd --version
```

### Test Locally

Run daprd locally:
```bash
./dist/linux_arm64/release/daprd \
  --app-id myapp \
  --dapr-http-port 3500 \
  --dapr-grpc-port 50001 \
  --log-level debug
```

## Troubleshooting

### Error: "cannot find package"

Make sure the durabletask-go replace directive is correct:
```bash
grep "durabletask-go" go.mod
```

### Error: "Go version mismatch"

Check your Go version:
```bash
go version
```

Update if needed:
```bash
# Using Homebrew on macOS
brew upgrade go
```

### Docker Build Fails

Check Docker daemon is running:
```bash
docker ps
```

Check disk space:
```bash
df -h
```

### Cross-Compilation Issues

If building for a different architecture than your host:

```bash
# Enable Docker buildx
docker buildx create --use

# Build with buildx
docker buildx build --platform linux/arm64 -t kaspernissen/daprd:tracing-v9-linux-arm64 .
```

## Build Targets

The Makefile provides several build targets:

```bash
make build              # Build all binaries
make docker-build       # Build Docker images
make docker-push        # Push Docker images
make release            # Build release binaries
make test               # Run tests
make lint               # Run linters
```

## Clean Build

To start fresh:

```bash
# Clean all build artifacts
make clean

# Remove Go cache
go clean -cache -modcache

# Rebuild
make build
```

## Build Performance

Typical build times:
- Binary build: ~2-5 minutes
- Docker build: ~3-7 minutes
- Full build + push: ~10-15 minutes

To speed up builds:
- Use cached Go modules: `go mod download`
- Parallel builds: `make -j4 build`
- Docker layer caching (automatic in most cases)

## Development Build

For faster development iteration (skip tests, optimization):

```bash
# Build without optimization (faster)
make DEBUG=1 build

# Build only daprd
make build-daprd
```

## CI/CD Integration

### GitHub Actions

```yaml
name: Build Dapr
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v2
      - uses: actions/setup-go@v2
        with:
          go-version: '1.21'
      - name: Build
        run: |
          export DAPR_REGISTRY=${{ secrets.DOCKER_USERNAME }}
          export DAPR_TAG=${{ github.sha }}
          make docker-build
      - name: Push
        run: |
          echo ${{ secrets.DOCKER_PASSWORD }} | docker login -u ${{ secrets.DOCKER_USERNAME }} --password-stdin
          make docker-push
```

## Version Information

- Dapr version: edge (based on commit 90a3bf1a4)
- Go version: 1.21+
- Docker image tag: `tracing-v9-linux-arm64`

## Next Steps

After building Dapr:

1. Deploy the updated daprd image to your Kubernetes cluster
2. Verify trace context propagation in logs
3. Check distributed traces in Jaeger/Zipkin
4. Monitor application metrics

## Related Documentation

- `WORKFLOW_ACTIVITY_TRACING.md` - Technical details of the implementation
- durabletask-go `BUILD.md` - Building the backend dependency
- durabletask-java `BUILD.md` - Building the Java client

## Registry Alternatives

### GitHub Container Registry

```bash
export DAPR_REGISTRY=ghcr.io/YOUR_USERNAME
docker login ghcr.io
make docker-build docker-push
```

### AWS ECR

```bash
aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin YOUR_ACCOUNT.dkr.ecr.us-east-1.amazonaws.com
export DAPR_REGISTRY=YOUR_ACCOUNT.dkr.ecr.us-east-1.amazonaws.com
make docker-build docker-push
```

### Google Container Registry

```bash
gcloud auth configure-docker
export DAPR_REGISTRY=gcr.io/YOUR_PROJECT
make docker-build docker-push
```
