package lambdaadapter

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-lambda-go/events"
)

// TaskFunc runs one background job invoked directly by the scheduler
// (payload {"task":"<name>"}), outside the Function URL path (M4b-1).
type TaskFunc func(ctx context.Context) error

// Mux dispatches raw Lambda payloads: task invocations to their TaskFunc,
// everything else to the buffered Function URL adapter. Function URL events
// never carry a top-level "task" key, so the probe is unambiguous.
type Mux struct {
	HTTP  *Handler
	Tasks map[string]TaskFunc
}

// Invoke is the Lambda handler entry point.
func (m *Mux) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var probe struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && probe.Task != "" {
		task, ok := m.Tasks[probe.Task]
		if !ok {
			return nil, fmt.Errorf("unknown task %q", probe.Task)
		}
		if err := task(ctx); err != nil {
			return nil, fmt.Errorf("task %s: %w", probe.Task, err)
		}
		return map[string]string{"task": probe.Task, "status": "ok"}, nil
	}
	var event events.LambdaFunctionURLRequest
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, fmt.Errorf("payload is neither a task nor a function url event: %w", err)
	}
	return m.HTTP.Invoke(ctx, event)
}
