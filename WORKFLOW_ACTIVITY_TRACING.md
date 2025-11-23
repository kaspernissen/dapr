# Workflow Activity Tracing Improvements

## Overview

This document describes the changes made to Dapr's workflow activity execution to improve trace context propagation and create proper parent-child relationships between producer and consumer spans.

## Problem Statement

The original implementation used span links to connect workflow orchestrator producer spans with activity consumer spans, but this approach had limitations:

1. Span links create a "related to" relationship, not a parent-child hierarchy
2. The workflow trace context was not being continued into activity execution
3. Consumer spans appeared as separate traces rather than continuing the workflow trace

## Solution

Replace span links with trace context continuation using `ContextWithRemoteSpanContext()` to maintain the parent-child relationship throughout the workflow execution.

## Changes Made

### File: `pkg/actors/targets/workflow/orchestrator/activity.go`

This file handles activity scheduling from the workflow orchestrator side (producer).

#### Changed: Trace Context Continuation Instead of Links

**Before:**
```go
// Use stored trace context as span link for activity scheduling span
var spanLinks []trace.Link
if state.TraceContext != nil {
    // ... parse trace context ...
    linkedSpanCtx := trace.NewSpanContext(trace.SpanContextConfig{
        TraceID:    traceID,
        SpanID:     spanID,
        TraceFlags: flags,
        TraceState: traceState,
        Remote:     true,
    })

    spanLinks = append(spanLinks, trace.Link{
        SpanContext: linkedSpanCtx,
        Attributes: []attribute.KeyValue{
            attribute.String("link.type", "follows_from"),
            attribute.String("workflow.instance.id", o.actorID),
        },
    })
}

ctx, span := otel.Tracer("dapr-workflow-orchestrator").Start(ctx, spanName,
    trace.WithSpanKind(trace.SpanKindProducer),
    trace.WithLinks(spanLinks...),
)
```

**After:**
```go
// Use stored trace context to continue the workflow trace
if state.TraceContext != nil {
    tc := state.TraceContext
    traceID, err1 := trace.TraceIDFromHex(tc.TraceID)
    spanID, err2 := trace.SpanIDFromHex(tc.SpanID)
    if err1 == nil && err2 == nil {
        var flags trace.TraceFlags
        fmt.Sscanf(tc.TraceFlags, "%02x", &flags)

        var traceState trace.TraceState
        if tc.TraceState != "" {
            traceState, _ = trace.ParseTraceState(tc.TraceState)
        }

        workflowSpanCtx := trace.NewSpanContext(trace.SpanContextConfig{
            TraceID:    traceID,
            SpanID:     spanID,
            TraceFlags: flags,
            TraceState: traceState,
            Remote:     true,
        })

        // Inject workflow span context as parent to continue the trace
        ctx = trace.ContextWithRemoteSpanContext(ctx, workflowSpanCtx)

        log.Infof("Workflow actor '%s': CONTINUING workflow trace for activity '%s': traceID=%s spanID=%s flags=%s",
            o.actorID, activityName, traceID, spanID, flags)
    }
}

ctx, span := otel.Tracer("dapr-workflow-orchestrator").Start(ctx, spanName,
    trace.WithSpanKind(trace.SpanKindProducer),
)
```

**Key Changes:**
- Renamed `linkedSpanCtx` to `workflowSpanCtx` for clarity
- Replaced `trace.WithLinks()` with `trace.ContextWithRemoteSpanContext()`
- Producer span now has the workflow span as its parent
- Removed span link attributes

### File: `pkg/actors/targets/workflow/activity/execute.go`

This file handles activity execution on the consumer side.

#### Changed: Continue Trace from Producer Span

**Before:**
```go
// Start consumer span for activity execution with link to producer
spanName := fmt.Sprintf("process %s", activityName)
ctx, span := otel.Tracer("dapr-workflow-activity").Start(ctx, spanName,
    trace.WithSpanKind(trace.SpanKindConsumer),
    trace.WithLinks(spanLinks...),
)
```

**After:**
```go
// Start consumer span for activity execution with link to producer
// If we have a producer span context, use it as the parent to continue the trace
if len(spanLinks) > 0 {
    // Inject the producer span context as remote parent to continue the trace
    ctx = trace.ContextWithRemoteSpanContext(ctx, spanLinks[0].SpanContext)
}

spanName := fmt.Sprintf("process %s", activityName)
ctx, span := otel.Tracer("dapr-workflow-activity").Start(ctx, spanName,
    trace.WithSpanKind(trace.SpanKindConsumer),
    trace.WithLinks(spanLinks...),
)
defer func() {
    span.End()
}()

consumerSpanCtx := span.SpanContext()
log.Infof("Activity actor '%s': CREATED CONSUMER SPAN: spanName='%s' traceID=%s spanID=%s hasLinks=%d",
    a.actorID, spanName, consumerSpanCtx.TraceID(), consumerSpanCtx.SpanID(), len(spanLinks))

// The consumer span context is already active in ctx and will be
// automatically propagated via gRPC metadata by the OTel instrumentation
log.Infof("Activity actor '%s': Consumer span context will be propagated via gRPC metadata: traceID=%s spanID=%s",
    a.actorID, consumerSpanCtx.TraceID(), consumerSpanCtx.SpanID())
```

**Key Changes:**
- Added `ContextWithRemoteSpanContext()` to set producer span as parent
- Consumer span now continues the trace hierarchy
- Added logging for consumer span creation
- Note: Span links are still kept for additional context

### File: `go.mod` and `go.sum`

Updated durabletask-go dependency to include trace context extraction changes:

```go
replace github.com/dapr/durabletask-go => /Users/kaspernissen/dash0/dapr/newnewclone/durabletask-go
```

## Architecture

### Trace Hierarchy

```
HTTP Request Span (from app)
  └─ Workflow Execution Span (stored in state.TraceContext)
       └─ schedule {activity} (Producer span)
            └─ process {activity} (Consumer span) ← Dapr creates this
                 └─ gRPC GetWorkItems call (with trace context in protobuf)
                      └─ Java Activity Execution
                           └─ HTTP Client Span (auto-instrumented)
```

### Trace Context Flow

1. **HTTP Request** → Workflow starts with trace context
2. **Workflow State** → Stores trace context (TraceID, SpanID, TraceFlags)
3. **callActivity()** → Injects workflow span context as remote parent
4. **Producer Span** → Created with workflow span as parent
5. **executeActivity()** → Injects producer span context as remote parent
6. **Consumer Span** → Created with producer span as parent
7. **durabletask-go** → Extracts consumer span context from ctx
8. **ActivityRequest** → Embeds trace context in protobuf
9. **Java Client** → Extracts and makes trace context current
10. **HTTP Calls** → Auto-instrumented with correct parent span

## Testing

To verify the implementation is working:

### 1. Check Producer Span Creation

```bash
kubectl logs -l app=pizza-store-service -c daprd | grep "CREATED PRODUCER SPAN"
```

Expected output:
```
CREATED PRODUCER SPAN for activity 'com.salaboy.pizza.store.workflow.PlaceOrderToKitchen':
spanName='schedule com.salaboy.pizza.store.workflow.PlaceOrderToKitchen'
traceID=c57c8abc4c298ffd0b9f28e3c7f61a66 spanID=2f0fc488b85a8bc1
```

### 2. Check Consumer Span Creation

```bash
kubectl logs -l app=pizza-store-service -c daprd | grep "CREATED CONSUMER SPAN"
```

Expected output:
```
CREATED CONSUMER SPAN: spanName='process com.salaboy.pizza.store.workflow.PlaceOrderToKitchen'
traceID=c57c8abc4c298ffd0b9f28e3c7f61a66 spanID=b849bd4a0181444d hasLinks=1
```

### 3. Verify Trace Continuity

All spans should have the same traceID: `c57c8abc4c298ffd0b9f28e3c7f61a66`

### 4. Check Trace in Jaeger

```bash
# View trace in Jaeger UI
# http://localhost:16686/trace/{traceId}
```

Expected hierarchy:
```
POST /order
  └─ workflow execution
       └─ schedule PlaceOrderToKitchen
            └─ process PlaceOrderToKitchen
                 └─ PUT /prepare (HTTP client span)
```

## Build Instructions

```bash
# Build Dapr with workflow tracing improvements
cd /Users/kaspernissen/dash0/dapr/newnewclone/dapr
export DAPR_REGISTRY=kaspernissen
export DAPR_TAG=tracing-v9
export TARGET_OS=linux
export TARGET_ARCH=arm64
make docker-build

# Push to registry
docker push kaspernissen/daprd:tracing-v9-linux-arm64
```

## Deployment

Update Dapr sidecar annotation in Kubernetes deployments:

```yaml
annotations:
  dapr.io/sidecar-image: kaspernissen/daprd:tracing-v9-linux-arm64-linux-arm64
```

## Key Insights

### Why ContextWithRemoteSpanContext Works

1. **Proper Parent-Child Relationships**: Instead of creating a "related to" link, we establish a true parent-child hierarchy in the trace tree.

2. **Trace Continuity**: All spans share the same traceID, ensuring they appear in the same distributed trace.

3. **Remote Span Context**: Using `Remote: true` in SpanContextConfig indicates the span context came from a different process, which is correct for workflow state restoration.

4. **OTel Auto-Instrumentation**: When the consumer span context is active in ctx, OTel gRPC interceptors automatically propagate it to durabletask-go via gRPC metadata. However, since GetWorkItems is a streaming RPC, we also embed it in the protobuf for per-work-item propagation.

### Why Span Links Are Still Used

Span links provide additional context about the relationship between producer and consumer spans, even though we now use parent-child relationships. They're useful for:
- Debugging and understanding the flow
- Correlation across different trace backends
- Additional metadata via link attributes

## Dependencies

This implementation requires:
- durabletask-go with trace context extraction (see TRACE_CONTEXT_PROPAGATION.md in durabletask-go repo)
- durabletask-java with trace context parsing (see TRACE_CONTEXT_PROPAGATION.md in durabletask-java repo)
- OpenTelemetry configuration with sampling rate = 1

## Version

- Dapr version: edge (based on commit 90a3bf1a4)
- Docker image: `kaspernissen/daprd:tracing-v9-linux-arm64`

## References

- OpenTelemetry Go SDK: https://pkg.go.dev/go.opentelemetry.io/otel/trace
- W3C Trace Context: https://www.w3.org/TR/trace-context/
- OpenTelemetry Semantic Conventions: https://opentelemetry.io/docs/specs/semconv/
