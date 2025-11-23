# Complete Build Guide: Workflow Activity Tracing

## Overview

This document provides a complete, end-to-end guide for building all components required for workflow activity tracing with trace context propagation.

## Architecture Overview

The trace context propagation feature spans three repositories:

1. **durabletask-go** - Extracts trace context from Dapr consumer span and embeds in protobuf
2. **durabletask-java** - Extracts trace context from protobuf and makes it current during activity execution
3. **dapr** - Creates consumer spans and uses durabletask-go for activity scheduling

## Prerequisites

- Go 1.21+
- Java 11 (not Java 25!)
- Gradle 7.x+
- Docker
- Make
- SDKMAN (optional, for Java version management)
- Container registry access (Docker Hub, etc.)

## Build Order

**IMPORTANT**: Build in this exact order due to dependencies:

```
durabletask-go → dapr → durabletask-java → applications
```

## Complete Build Process

### Step 1: Build durabletask-go

```bash
# Navigate to durabletask-go
cd /Users/kaspernissen/dash0/dapr/newnewclone/durabletask-go

# Verify changes
git status
# Should show: backend/executor.go

# Build
go build ./...

# Verify
grep -n "extractTraceContextFromCtx" backend/executor.go
# Should show function at line ~275
```

**Changes**: Added trace context extraction from Go context and embedding in ActivityRequest protobuf.

**Time**: ~30 seconds

### Step 2: Build Dapr

```bash
# Navigate to dapr
cd /Users/kaspernissen/dash0/dapr/newnewclone/dapr

# Verify durabletask-go replace directive
grep "durabletask-go" go.mod
# Should show: replace github.com/dapr/durabletask-go => /Users/kaspernissen/dash0/dapr/newnewclone/durabletask-go

# Verify changes
git status
# Should show:
#   - pkg/actors/targets/workflow/activity/execute.go
#   - pkg/actors/targets/workflow/orchestrator/activity.go
#   - go.mod, go.sum

# Set environment variables
export DAPR_REGISTRY=kaspernissen
export DAPR_TAG=tracing-v9
export TARGET_OS=linux
export TARGET_ARCH=arm64

# Build binaries
make build

# Build Docker image
make docker-build

# Login to Docker Hub (if needed)
docker login

# Push image
docker push kaspernissen/daprd:tracing-v9-linux-arm64
```

**Changes**: Modified workflow orchestrator and activity execution to continue traces instead of using span links.

**Time**: ~5-10 minutes

### Step 3: Build durabletask-java

```bash
# Navigate to durabletask-java
cd /Users/kaspernissen/dash0/dapr/newnewclone/durabletask-java

# Verify changes
git status
# Should show:
#   - client/build.gradle
#   - client/src/main/java/io/dapr/durabletask/DurableTaskGrpcWorker.java

# Set Java 11
export JAVA_HOME=~/.sdkman/candidates/java/11.0.27-tem
export PATH=$JAVA_HOME/bin:$PATH
java -version
# Should show: openjdk version "11.0.27"

# Build and publish to Maven local
./gradlew clean build publishToMavenLocal -x test

# Verify publication
ls -la ~/.m2/repository/io/dapr/durabletask-client/1.5.13-SNAPSHOT/
# Should show JAR files
```

**Changes**: Added trace context extraction from protobuf and makeCurrent() with try-with-resources.

**Time**: ~15-30 seconds

### Step 4: Build Applications (Example: pizza-store)

```bash
# Navigate to pizza-store
cd /Users/kaspernissen/dash0/dapr/pizza/pizza-store

# Update pom.xml to use durabletask-java 1.5.13-SNAPSHOT
# This should already be set if you haven't changed it

# Ensure Java 21 (not 25!)
grep "java.version" pom.xml
# Should show: <java.version>21</java.version>

# Build JAR
mvn clean package -DskipTests

# Build Docker image
docker build -f Dockerfile.v7 -t kaspernissen/pizza-store:1.0.14-agentic-tracing-v8-arm64 .

# Push Docker image
docker push kaspernissen/pizza-store:1.0.14-agentic-tracing-v8-arm64
```

**Time**: ~2-3 minutes

### Step 5: Deploy to Kubernetes

```bash
# Update deployment files to use new images
cd /Users/kaspernissen/dash0/dapr/pizza/k8s

# pizza-store.yaml should have:
#   dapr.io/sidecar-image: kaspernissen/daprd:tracing-v9-linux-arm64-linux-arm64
#   image: kaspernissen/pizza-store:1.0.14-agentic-tracing-v8-arm64

# pizza-kitchen.yaml should have:
#   dapr.io/sidecar-image: kaspernissen/daprd:tracing-v9-linux-arm64-linux-arm64

# pizza-delivery.yaml should have:
#   dapr.io/sidecar-image: kaspernissen/daprd:tracing-v9-linux-arm64-linux-arm64

# Apply deployments
kubectl apply -f pizza-store.yaml
kubectl apply -f pizza-kitchen.yaml
kubectl apply -f pizza-delivery.yaml

# Watch rollout
kubectl rollout status deployment/pizza-store-deployment
kubectl rollout status deployment/pizza-kitchen-deployment
kubectl rollout status deployment/pizza-delivery-deployment
```

**Time**: ~2-3 minutes for pod restart

## Verification

### 1. Check Trace Context Extraction (durabletask-go → durabletask-java)

```bash
kubectl logs -l app=pizza-store-service -c pizza-store-service | grep "Extracting trace context"
```

Expected:
```
Extracting trace context from ActivityRequest: traceparent=00-{traceId}-{spanId}-01
Extracted trace context: PropagatedSpan{ImmutableSpanContext{traceId=..., spanId=...}}
```

### 2. Check Consumer Span Creation (Dapr)

```bash
kubectl logs -l app=pizza-store-service -c daprd | grep "CREATED CONSUMER SPAN"
```

Expected:
```
CREATED CONSUMER SPAN: spanName='process com.salaboy.pizza.store.workflow.PlaceOrderToKitchen'
traceID=... spanID=... hasLinks=1
```

### 3. Check Traces in Jaeger

```bash
# Port-forward Jaeger
kubectl port-forward -n opentelemetry svc/jaeger 16686:16686

# Open in browser
open http://localhost:16686
```

Expected trace structure:
```
POST /order
  └─ workflow execution
       └─ schedule PlaceOrderToKitchen
            └─ process PlaceOrderToKitchen (Dapr consumer span)
                 └─ PUT /prepare (HTTP client span) ← Should now appear!
                      └─ CallLocal/kitchen-service/prepare
```

## Quick Build Script

For convenience, here's a complete build script:

```bash
#!/bin/bash
set -e

echo "=== Building durabletask-go ==="
cd /Users/kaspernissen/dash0/dapr/newnewclone/durabletask-go
go build ./...

echo "=== Building Dapr ==="
cd /Users/kaspernissen/dash0/dapr/newnewclone/dapr
export DAPR_REGISTRY=kaspernissen
export DAPR_TAG=tracing-v9
export TARGET_OS=linux
export TARGET_ARCH=arm64
make docker-build
docker push kaspernissen/daprd:tracing-v9-linux-arm64

echo "=== Building durabletask-java ==="
cd /Users/kaspernissen/dash0/dapr/newnewclone/durabletask-java
export JAVA_HOME=~/.sdkman/candidates/java/11.0.27-tem
export PATH=$JAVA_HOME/bin:$PATH
./gradlew clean build publishToMavenLocal -x test

echo "=== Building pizza-store ==="
cd /Users/kaspernissen/dash0/dapr/pizza/pizza-store
mvn clean package -DskipTests
docker build -f Dockerfile.v7 -t kaspernissen/pizza-store:1.0.14-agentic-tracing-v8-arm64 .
docker push kaspernissen/pizza-store:1.0.14-agentic-tracing-v8-arm64

echo "=== Deploying to Kubernetes ==="
cd /Users/kaspernissen/dash0/dapr/pizza/k8s
kubectl apply -f pizza-store.yaml
kubectl apply -f pizza-kitchen.yaml
kubectl apply -f pizza-delivery.yaml

echo "=== Done! ==="
echo "Watch rollout: kubectl rollout status deployment/pizza-store-deployment"
echo "Check logs: kubectl logs -l app=pizza-store-service -c daprd --tail=100"
echo "View traces: http://localhost:16686 (after port-forwarding Jaeger)"
```

Save as `build-all.sh` and run:
```bash
chmod +x build-all.sh
./build-all.sh
```

## Troubleshooting

### "Java version 25 is not supported"

**Problem**: OTel Java agent requires Java 21 or lower.

**Solution**:
```bash
# Check pom.xml
grep "java.version" pizza-store/pom.xml
# Should be 21, not 25

# Set JAVA_HOME
export JAVA_HOME=~/.sdkman/candidates/java/21.0.x-tem
```

### "cannot find durabletask-go"

**Problem**: Dapr can't find the local durabletask-go.

**Solution**:
```bash
cd /Users/kaspernissen/dash0/dapr/newnewclone/dapr
grep "durabletask-go" go.mod
# Should show replace directive

# If not, add:
# replace github.com/dapr/durabletask-go => /Users/kaspernissen/dash0/dapr/newnewclone/durabletask-go
```

### "durabletask-client 1.5.13-SNAPSHOT not found"

**Problem**: Maven can't find the durabletask-java library.

**Solution**:
```bash
# Rebuild and publish durabletask-java
cd /Users/kaspernissen/dash0/dapr/newnewclone/durabletask-java
./gradlew clean publishToMavenLocal -x test

# Verify
ls ~/.m2/repository/io/dapr/durabletask-client/1.5.13-SNAPSHOT/
```

### Pods stuck in ImagePullBackOff

**Problem**: Kubernetes can't pull the Docker images.

**Solution**:
```bash
# Check if images exist
docker images | grep tracing-v9

# Push again if needed
docker push kaspernissen/daprd:tracing-v9-linux-arm64
docker push kaspernissen/pizza-store:1.0.14-agentic-tracing-v8-arm64

# For private registries, create image pull secret
kubectl create secret docker-registry regcred \
  --docker-server=docker.io \
  --docker-username=kaspernissen \
  --docker-password=YOUR_PASSWORD
```

### Trace context not appearing in logs

**Problem**: No "Extracting trace context" logs.

**Solution**:
```bash
# Check if the new code is deployed
kubectl logs -l app=pizza-store-service -c pizza-store-service | grep "DurableTaskGrpcWorker"

# Check if durabletask-java version is correct
kubectl exec -it pizza-store-deployment-xxx -c pizza-store-service -- \
  find /workspace -name "durabletask-client-*.jar"

# Restart deployment if needed
kubectl rollout restart deployment/pizza-store-deployment
```

## Summary

This complete build process:

1. ✅ **durabletask-go**: Extracts trace context from consumer span → embeds in protobuf
2. ✅ **dapr**: Creates consumer spans with proper parent-child relationships
3. ✅ **durabletask-java**: Extracts trace context from protobuf → makes it current
4. ✅ **applications**: HTTP calls auto-instrumented with correct trace context

Result: **Complete distributed trace from HTTP request → workflow → activities → HTTP client calls**

## Build Time Summary

- durabletask-go: ~30 seconds
- dapr: ~5-10 minutes
- durabletask-java: ~15-30 seconds
- pizza-store: ~2-3 minutes
- Kubernetes deployment: ~2-3 minutes

**Total**: ~10-20 minutes

## Version Information

- durabletask-go: main branch
- dapr: edge (based on commit 90a3bf1a4)
- durabletask-java: 1.5.13-SNAPSHOT
- Docker images:
  - `kaspernissen/daprd:tracing-v9-linux-arm64`
  - `kaspernissen/pizza-store:1.0.14-agentic-tracing-v8-arm64`

## Related Documentation

- `durabletask-go/TRACE_CONTEXT_PROPAGATION.md` - Technical implementation details
- `durabletask-go/BUILD.md` - Detailed build instructions
- `durabletask-java/TRACE_CONTEXT_PROPAGATION.md` - Java client implementation
- `durabletask-java/BUILD.md` - Detailed build instructions
- `dapr/WORKFLOW_ACTIVITY_TRACING.md` - Workflow tracing improvements
- `dapr/BUILD.md` - Detailed build instructions

## Next Steps

1. Test workflow execution and verify traces
2. Monitor application logs for errors
3. Check Jaeger for complete distributed traces
4. Document any issues or improvements
5. Create pull requests for upstream contribution
