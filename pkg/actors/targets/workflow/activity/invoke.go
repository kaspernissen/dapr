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

package activity

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	actorapi "github.com/dapr/dapr/pkg/actors/api"
	invokev1 "github.com/dapr/dapr/pkg/messaging/v1"
	internalsv1pb "github.com/dapr/dapr/pkg/proto/internals/v1"
	wferrors "github.com/dapr/dapr/pkg/runtime/wfengine/errors"
	"github.com/dapr/dapr/pkg/runtime/wfengine/todo"
	"github.com/dapr/durabletask-go/backend"
)

// Activities are scheduled by workflows and can execute for arbitrary lengths of time. Instead of executing
// activity logic directly, InvokeMethod creates a reminder that executes the activity logic. InvokeMethod
// returns immediately after creating the reminder, enabling the workflow to continue processing other events
// in parallel.
func (a *activity) handleInvoke(ctx context.Context, req *internalsv1pb.InternalInvokeRequest) (*internalsv1pb.InternalInvokeResponse, error) {
	method := req.GetMessage().GetMethod()

	dueTime := time.Now()
	if s, ok := req.GetMetadata()[todo.MetadataActivityReminderDueTime]; ok && len(s.GetValues()) > 0 {
		unix, err := strconv.ParseInt(s.GetValues()[0], 10, 64)
		if err != nil {
			return nil, err
		}
		dueTime = time.UnixMilli(unix)
	}

	log.Debugf("Activity actor '%s': invoking method '%s'", a.actorID, method)

	imReq, err := invokev1.FromInternalInvokeRequest(req)
	if err != nil {
		return nil, fmt.Errorf("failed to create InvokeMethodRequest: %w", err)
	}
	defer imReq.Close()

	msg := imReq.Message()

	var his backend.HistoryEvent
	if err = proto.Unmarshal(msg.GetData().GetValue(), &his); err != nil {
		return nil, fmt.Errorf("failed to decode activity request: %w", err)
	}

	// Extract and store trace context metadata for span linking
	if req.Metadata != nil {
		traceCtx := make(map[string]string)
		if traceID := req.Metadata["wf-trace-id"]; traceID != nil && len(traceID.Values) > 0 {
			traceCtx["trace-id"] = traceID.Values[0]
		}
		if spanID := req.Metadata["wf-span-id"]; spanID != nil && len(spanID.Values) > 0 {
			traceCtx["span-id"] = spanID.Values[0]
		}
		if traceFlags := req.Metadata["wf-trace-flags"]; traceFlags != nil && len(traceFlags.Values) > 0 {
			traceCtx["trace-flags"] = traceFlags.Values[0]
		}
		if traceState := req.Metadata["wf-trace-state"]; traceState != nil && len(traceState.Values) > 0 {
			traceCtx["trace-state"] = traceState.Values[0]
		}
		if len(traceCtx) > 0 {
			a.traceContexts.Store(a.actorID, traceCtx)
			log.Infof("Activity actor '%s': EXTRACTED trace context from metadata: traceID=%s spanID=%s flags=%s",
				a.actorID, traceCtx["trace-id"], traceCtx["span-id"], traceCtx["trace-flags"])
		} else {
			log.Warnf("Activity actor '%s': NO TRACE CONTEXT found in metadata", a.actorID)
		}
	} else {
		log.Warnf("Activity actor '%s': NO METADATA in request", a.actorID)
	}

	// The actual execution is triggered by a reminder
	return nil, a.createReminder(ctx, &his, dueTime)
}

func (a *activity) handleReminder(ctx context.Context, reminder *actorapi.Reminder) error {
	log.Infof("Activity actor '%s': REMINDER TRIGGERED: name='%s'", a.actorID, reminder.Name)

	var state backend.HistoryEvent
	if err := reminder.Data.UnmarshalTo(&state); err != nil {
		return fmt.Errorf("failed to decode activity reminder: %w", err)
	}

	// Retrieve trace context for span linking
	var traceCtx map[string]string
	if val, ok := a.traceContexts.Load(a.actorID); ok {
		traceCtx = val.(map[string]string)
		log.Infof("Activity actor '%s': RETRIEVED stored trace context: traceID=%s spanID=%s",
			a.actorID, traceCtx["trace-id"], traceCtx["span-id"])
	} else {
		log.Warnf("Activity actor '%s': NO STORED TRACE CONTEXT found for reminder", a.actorID)
	}

	log.Infof("Activity actor '%s': CALLING executeActivity with traceCtx=%v", a.actorID, traceCtx)
	err := a.executeActivity(ctx, reminder.Name, &state, traceCtx)

	// Returning nil signals that we want the execution to be retried in the next
	// period interval
	switch {
	case err == nil:
		// Clean up trace context on successful completion
		a.traceContexts.Delete(a.actorID)
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		log.Warnf("%s: execution of '%s' timed-out and will be retried later: %v", a.actorID, reminder.Name, err)
		return err
	case errors.Is(err, context.Canceled):
		log.Warnf("%s: received cancellation signal while waiting for activity execution '%s'", a.actorID, reminder.Name)
		return err
	case wferrors.IsRecoverable(err):
		log.Warnf("%s: execution failed with a recoverable error and will be retried later: %v", a.actorID, err)
		return err
	default: // Other error
		log.Errorf("%s: execution failed with an error: %v", a.actorID, err)
		return err
	}
}
