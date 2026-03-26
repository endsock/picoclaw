package taskdb

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/tools"
)

type NoopRecorder struct{}

func NewNoopRecorder() *NoopRecorder {
	return &NoopRecorder{}
}

func (n *NoopRecorder) CreateSubmitted(context.Context, *tools.SubagentTaskRecord) error {
	return nil
}

func (n *NoopRecorder) MarkRunning(context.Context, string, int64) error {
	return nil
}

func (n *NoopRecorder) FinishTask(context.Context, string, *tools.SubagentTaskFinishPatch) error {
	return nil
}
