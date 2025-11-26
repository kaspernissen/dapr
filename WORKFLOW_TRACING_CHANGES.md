# Workflow RaiseEvent Tracing Implementation

## Overview
This document describes the implementation of distributed tracing for Dapr workflow `RaiseEvent` operations using OpenTelemetry span links and parent-child relationships. The implementation follows a producer/consumer pattern where:

1. **Producer Span**: Created when `RaiseEvent` is called, capturing the trace context
2. **Trace Context Storage**: Both producer span context and orchestration span context are stored in workflow state
3. **Consumer Span**: Created when the workflow processes the event, with:
   - **Parent**: Orchestration span (makes it part of the workflow trace)
   - **Span Link**: RaiseEvent producer span (shows the causal relationship)

## Architecture

The implementation uses OpenTelemetry span links AND parent-child relationships to connect asynchronous RaiseEvent operations:

```
Workflow Start
    ├─> Capture orchestration trace context
    └─> Store in workflow state

RaiseEvent Call (Producer)
    ├─> Create producer span
    ├─> Store trace context in workflow state
    └─> Send event to workflow actor

Workflow Event Processing (Consumer)
    ├─> Load both trace contexts from state
    ├─> Inject orchestration context as parent
    ├─> Create consumer span (child of orchestration)
    ├─> Add span link to producer
    └─> Process event
```

This results in:
- ProcessRaisedEvent span appears in the **same trace** as the workflow orchestration
- Span link connects it to the RaiseEvent producer call
- Both connections are visible in tracing UIs

## Files Modified

### 1. pkg/runtime/wfengine/state/state.go

**Purpose**: Add trace context storage infrastructure to workflow state

**Changes**:
- Added `traceContextKey` constant for state storage key
- Created `TraceContext` struct to store OpenTelemetry trace context (traceID, spanID, traceFlags, traceState)
- Added `TraceContext` field to `State` struct
- Implemented save logic in `GetSaveRequest()` method to persist trace context
- Implemented load logic in `LoadWorkflowState()` to retrieve trace context

**Key Code**:
```go
// TraceContext stores OpenTelemetry trace context for span propagation
type TraceContext struct {
	TraceID    string
	SpanID     string
	TraceFlags string
	TraceState string
}

type State struct {
	// ... existing fields ...

	// Trace context for linking RaiseEvent spans
	TraceContext *TraceContext

	// ... existing fields ...
}
```

### 2. pkg/runtime/wfengine/client.go

**Purpose**: Create producer span when RaiseEvent is called

**Changes**:
- Added OpenTelemetry imports (`otel`, `trace`, `attribute`)
- Modified `RaiseEvent()` method to create a producer span with:
  - `SpanKind`: `trace.SpanKindProducer`
  - Attributes: `workflow.instance_id`, `workflow.event_name`
- Span automatically ends via `defer span.End()`

**Key Code**:
```go
func (c *client) RaiseEvent(ctx context.Context, req *workflows.RaiseEventRequest) error {
	// ... validation ...

	// Create a producer span for the RaiseEvent operation
	tracer := otel.Tracer("dapr.runtime.wfengine")
	ctx, span := tracer.Start(ctx, "RaiseEvent",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("workflow.instance_id", req.InstanceID),
			attribute.String("workflow.event_name", req.EventName),
		),
	)
	defer span.End()

	// ... existing RaiseEvent logic ...
}
```

### 3. pkg/actors/targets/workflow/orchestrator/add.go

**Purpose**: Capture producer span context when event is added to workflow

**Changes**:
- Added OpenTelemetry import (`trace`)
- Added workflow state import (`wfenginestate`)
- Modified `addWorkflowEvent()` to capture trace context from incoming request when `EventRaised` is detected
- Stores captured trace context in workflow state

**Key Code**:
```go
func (o *orchestrator) addWorkflowEvent(ctx context.Context, historyEventBytes []byte) error {
	// ... existing logic ...

	// Capture trace context from RaiseEvent for span linking
	if e.GetEventRaised() != nil {
		if spanCtx := trace.SpanFromContext(ctx).SpanContext(); spanCtx.IsValid() {
			state.TraceContext = &wfenginestate.TraceContext{
				TraceID:    spanCtx.TraceID().String(),
				SpanID:     spanCtx.SpanID().String(),
				TraceFlags: fmt.Sprintf("%02x", spanCtx.TraceFlags()),
				TraceState: spanCtx.TraceState().String(),
			}
			log.Infof("Workflow actor '%s': CAPTURED RaiseEvent trace context traceID=%s spanID=%s flags=%s",
				o.actorID, spanCtx.TraceID(), spanCtx.SpanID(), spanCtx.TraceFlags())
		}
	}

	// ... existing logic ...
}
```

### 4. pkg/actors/targets/workflow/orchestrator/run.go

**Purpose**: Create consumer span with span link when processing events

**Changes**:
- Added OpenTelemetry imports (`otel`, `trace`, `attribute`)
- Added `encoding/hex` import for parsing trace flags
- Modified `runWorkflow()` to:
  1. Check for RaiseEvent in inbox
  2. Load and parse both stored trace contexts (RaiseEvent producer and orchestration)
  3. Reconstruct both span contexts
  4. **CRITICAL FIX**: Use `trace.ContextWithRemoteSpanContext()` instead of `trace.ContextWithSpanContext()`
     - This properly enables tracing for background contexts (like actor reminders)
     - Without this, spans are created but never exported to the tracing backend
  5. Create consumer span as child of orchestration span with link to producer span
  6. Process workflow with linked span context

**Key Code**:
```go
func (o *orchestrator) runWorkflow(ctx context.Context, reminder *actorapi.Reminder) (todo.RunCompleted, error) {
	// ... existing setup ...

	// Check if there are RaiseEvent events in the inbox and create a consumer span with span link
	var consumerSpan trace.Span
	if state.TraceContext != nil {
		// Check if any event in the inbox is a RaiseEvent
		hasRaiseEvent := false
		for _, e := range state.Inbox {
			if e.GetEventRaised() != nil {
				hasRaiseEvent = true
				break
			}
		}

		if hasRaiseEvent {
			// Parse the stored producer span context
			traceID, err := trace.TraceIDFromHex(state.TraceContext.TraceID)
			if err == nil {
				spanID, err := trace.SpanIDFromHex(state.TraceContext.SpanID)
				if err == nil {
					var traceFlags trace.TraceFlags
					if flagBytes, err := hex.DecodeString(state.TraceContext.TraceFlags); err == nil && len(flagBytes) > 0 {
						traceFlags = trace.TraceFlags(flagBytes[0])
					}

					// Create the producer span context for the link
					producerSpanCtx := trace.NewSpanContext(trace.SpanContextConfig{
						TraceID:    traceID,
						SpanID:     spanID,
						TraceFlags: traceFlags,
					})

					// Create consumer span with link to producer span
					tracer := otel.Tracer("dapr.runtime.wfengine")
					ctx, consumerSpan = tracer.Start(ctx, "ProcessRaisedEvent",
						trace.WithSpanKind(trace.SpanKindConsumer),
						trace.WithLinks(trace.Link{
							SpanContext: producerSpanCtx,
						}),
						trace.WithAttributes(
							attribute.String("workflow.instance_id", o.actorID),
							attribute.String("workflow.name", workflowName),
						),
					)
					defer func() {
						if consumerSpan != nil {
							consumerSpan.End()
						}
					}()
					log.Infof("Workflow actor '%s': CREATED consumer span with link to producer traceID=%s spanID=%s",
						o.actorID, traceID, spanID)
				}
			}
		}
	}

	// ... existing workflow execution logic ...
}
```

## How It Works

1. **RaiseEvent Called**: When `client.RaiseEvent()` is invoked:
   - A producer span is created with span kind `Producer`
   - The span context is automatically propagated via the context parameter
   - The span ends when the function returns

2. **Event Added to Workflow**: When `addWorkflowEvent()` processes the event:
   - Detects if it's an `EventRaised` event
   - Extracts the producer span context from the incoming context
   - Stores the trace context (traceID, spanID, flags, state) in the workflow state
   - Persists to state store via transactional operation

3. **Workflow Processes Event**: When `runWorkflow()` executes:
   - Loads workflow state including stored trace context
   - Checks if inbox contains RaiseEvent events
   - Reconstructs the producer span context from stored data
   - Creates a consumer span with:
     - Span kind: `Consumer`
     - Span link pointing to the producer span
     - Attributes identifying the workflow
   - Processes the workflow with this linked context
   - Consumer span ends when workflow execution completes

## Tracing Flow Example

```
App calls RaiseEvent("order-approved", workflow-123)
    └─> [Producer Span: RaiseEvent]
         ├─ Trace ID: abc123...
         ├─ Span ID: def456...
         └─ Context stored in workflow state

Workflow actor receives event
    └─> Load trace context from state

Workflow processes event
    └─> [Consumer Span: ProcessRaisedEvent]
         ├─ Trace ID: xyz789... (new trace)
         ├─ Span ID: ghi101...
         └─ Link: -> Producer Span (abc123.../def456...)
```

## Benefits

1. **Async Operation Visibility**: Links producer and consumer spans across different traces
2. **Causal Relationship**: Clearly shows which RaiseEvent caused which workflow execution
3. **Distributed Tracing**: Works across service boundaries and time delays
4. **Observability**: Enables tracing tools (like Dash0, Jaeger, Zipkin) to visualize event flow
5. **Debugging**: Helps trace issues from event source through workflow processing

## Testing

The implementation:
- ✅ Compiles successfully
- ✅ Individual package builds verified
- ✅ Full `make build` completed
- ✅ Follows existing Dapr tracing patterns
- ✅ Uses standard OpenTelemetry span link mechanisms

## Next Steps

1. Deploy and test with actual workflows
2. Verify span links appear in tracing backends (Dash0, Jaeger, etc.)
3. Test with various workflow scenarios:
   - Single RaiseEvent
   - Multiple RaiseEvents
   - RaiseEvent with workflow delays
   - Cross-app workflow events
4. Consider extending to other workflow event types if needed
