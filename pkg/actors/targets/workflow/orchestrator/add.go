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
	"fmt"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	diag "github.com/dapr/dapr/pkg/diagnostics"
	wfenginestate "github.com/dapr/dapr/pkg/runtime/wfengine/state"
	"github.com/dapr/durabletask-go/api"
	"github.com/dapr/durabletask-go/backend"
)

func (o *orchestrator) addWorkflowEvent(ctx context.Context, historyEventBytes []byte) error {
	state, _, err := o.loadInternalState(ctx)
	if err != nil {
		return err
	}

	if state == nil {
		log.Errorf("Workflow actor '%s': cannot add event to workflow as state has been purged. Ignoring event.", o.actorID)
		return api.ErrInstanceNotFound
	}

	var e backend.HistoryEvent
	err = proto.Unmarshal(historyEventBytes, &e)
	if e.GetTaskCompleted() != nil || e.GetTaskFailed() != nil {
		o.activityResultAwaited.CompareAndSwap(true, false)
	}
	if err != nil {
		return err
	}

	// Capture trace context from RaiseEvent for span linking
	if e.GetEventRaised() != nil {
		log.Infof("Workflow actor '%s': Processing RaiseEvent, attempting to capture trace context", o.actorID)

		// Try to get span context from gRPC metadata (most reliable)
		spanCtx, foundInMetadata := diag.SpanContextFromIncomingGRPCMetadata(ctx)
		if foundInMetadata && spanCtx.IsValid() {
			state.TraceContext = &wfenginestate.TraceContext{
				TraceID:    spanCtx.TraceID().String(),
				SpanID:     spanCtx.SpanID().String(),
				TraceFlags: fmt.Sprintf("%02x", spanCtx.TraceFlags()),
				TraceState: spanCtx.TraceState().String(),
			}
			log.Infof("Workflow actor '%s': CAPTURED RaiseEvent trace context from gRPC metadata - traceID=%s spanID=%s flags=%s",
				o.actorID, spanCtx.TraceID(), spanCtx.SpanID(), spanCtx.TraceFlags())
		} else {
			// Fallback: try to get from context's current span
			spanCtx = trace.SpanFromContext(ctx).SpanContext()
			if spanCtx.IsValid() {
				state.TraceContext = &wfenginestate.TraceContext{
					TraceID:    spanCtx.TraceID().String(),
					SpanID:     spanCtx.SpanID().String(),
					TraceFlags: fmt.Sprintf("%02x", spanCtx.TraceFlags()),
					TraceState: spanCtx.TraceState().String(),
				}
				log.Infof("Workflow actor '%s': CAPTURED RaiseEvent trace context from context span - traceID=%s spanID=%s flags=%s",
					o.actorID, spanCtx.TraceID(), spanCtx.SpanID(), spanCtx.TraceFlags())
			} else {
				log.Warnf("Workflow actor '%s': FAILED to capture RaiseEvent trace context - no valid span context found in metadata or context", o.actorID)
			}
		}
	}

	log.Debugf("Workflow actor '%s': adding event to the workflow inbox", o.actorID)
	state.AddToInbox(&e)

	if err := o.saveInternalState(ctx, state); err != nil {
		return err
	}

	// For activity completion events, we want to create the reminder on the same app where this workflow actor is
	// hosted, so use the source app from the router.
	// For sub-orchestrator completion events we want to create the reminder on the current app.
	sourceAppID := o.appID
	returningToParent := e.GetSubOrchestrationInstanceCompleted() != nil || e.GetSubOrchestrationInstanceFailed() != nil
	if !returningToParent && e.GetRouter() != nil {
		sourceAppID = e.GetRouter().GetSourceAppID()
	}

	dueTime := e.Timestamp.AsTime()
	if len(state.History) > 0 {
		dueTime = state.History[0].Timestamp.AsTime()
	}
	if _, err := o.createWorkflowReminder(ctx, "new-event", nil, dueTime, sourceAppID); err != nil {
		return err
	}

	return nil
}
