/*
Copyright 2025 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	invokev1 "github.com/dapr/dapr/pkg/messaging/v1"
	internalsv1pb "github.com/dapr/dapr/pkg/proto/internals/v1"
	wfenginestate "github.com/dapr/dapr/pkg/runtime/wfengine/state"
	"github.com/dapr/dapr/pkg/runtime/wfengine/todo"
	"github.com/dapr/durabletask-go/backend"
)

func (o *orchestrator) callActivities(ctx context.Context, es []*backend.HistoryEvent, state *wfenginestate.State) error {
	var dueTime time.Time
	if len(state.History) > 0 {
		dueTime = state.History[0].GetTimestamp().AsTime()
	} else {
		dueTime = state.Inbox[0].GetTimestamp().AsTime()
	}

	for _, e := range es {
		err := o.callActivity(ctx, e, dueTime, state)
		if err != nil {
			if errors.Is(err, todo.ErrDuplicateInvocation) {
				log.Warnf("Workflow actor '%s': activity invocation '%s::%d' was flagged as a duplicate and will be skipped", o.actorID, e.GetTaskScheduled().GetName(), e.GetEventId())
				continue
			}

			return err
		}
	}

	return nil
}

func (o *orchestrator) callActivity(ctx context.Context, e *backend.HistoryEvent, dueTime time.Time, state *wfenginestate.State) error {
	ts := e.GetTaskScheduled()
	if ts == nil {
		log.Warnf("Workflow actor '%s': unable to process task '%v'", o.actorID, e)
		return nil
	}

	// Start producer span for activity scheduling
	activityName := ts.GetName()
	spanName := fmt.Sprintf("schedule %s", activityName)

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
		} else {
			log.Warnf("Workflow actor '%s': FAILED to parse stored trace context for activity '%s': traceErr=%v spanErr=%v",
				o.actorID, activityName, err1, err2)
		}
	} else {
		log.Warnf("Workflow actor '%s': NO STORED TRACE CONTEXT found for activity '%s'", o.actorID, activityName)
	}

	ctx, span := otel.Tracer("dapr-workflow-orchestrator").Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindProducer),
	)
	defer func() {
		span.End()
	}()

	producerSpanCtx := span.SpanContext()
	log.Infof("Workflow actor '%s': CREATED PRODUCER SPAN for activity '%s': spanName='%s' traceID=%s spanID=%s",
		o.actorID, activityName, spanName, producerSpanCtx.TraceID(), producerSpanCtx.SpanID())

	span.SetAttributes(
		attribute.String("messaging.operation.name", "publish"),
		attribute.String("workflow.activity.name", activityName),
		attribute.String("workflow.instance.id", o.actorID),
		attribute.Int("workflow.activity.event_id", int(e.GetEventId())),
		attribute.Int("workflow.generation", int(state.Generation)),
	)

	var eventData []byte
	eventData, err := proto.Marshal(e)
	if err != nil {
		span.RecordError(err)
		return err
	}

	activityActorType := o.activityActorType
	if router := e.GetRouter(); router != nil && router.TargetAppID != nil {
		activityActorType = o.actorTypeBuilder.Activity(router.GetTargetAppID())
	}

	targetActorID := buildActivityActorID(o.actorID, e.GetEventId(), state.Generation)

	o.activityResultAwaited.Store(true)

	log.Debugf("Workflow actor '%s': invoking execute method on activity actor '%s||%s'", o.actorID, activityActorType, targetActorID)

	// Inject trace context into metadata for span linking
	spanCtx := span.SpanContext()
	metadata := map[string][]string{
		todo.MetadataActivityReminderDueTime: {strconv.FormatInt(dueTime.UnixMilli(), 10)},
		"wf-trace-id":                        {spanCtx.TraceID().String()},
		"wf-span-id":                         {spanCtx.SpanID().String()},
		"wf-trace-flags":                     {fmt.Sprintf("%02x", spanCtx.TraceFlags())},
	}
	if spanCtx.TraceState().String() != "" {
		metadata["wf-trace-state"] = []string{spanCtx.TraceState().String()}
	}

	log.Infof("Workflow actor '%s': INJECTING trace context into metadata for activity '%s': traceID=%s spanID=%s flags=%s",
		o.actorID, activityName, spanCtx.TraceID(), spanCtx.SpanID(), spanCtx.TraceFlags())

	_, err = o.router.Call(ctx, internalsv1pb.
		NewInternalInvokeRequest("Execute").
		WithActor(activityActorType, targetActorID).
		WithMetadata(metadata).
		WithData(eventData).
		WithContentType(invokev1.ProtobufContentType),
	)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("failed to invoke activity actor '%s' to execute '%s': %w", targetActorID, ts.GetName(), err)
	}

	return nil
}

func buildActivityActorID(workflowID string, taskID int32, generation uint64) string {
	// An activity can be identified by its name followed by its task ID and generation. Example: SayHello::0::1, SayHello::1::1, etc.
	return workflowID + "::" + strconv.Itoa(int(taskID)) + "::" + strconv.FormatUint(generation, 10)
}
