package taskdb

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/tools"
)

func TestNoopRecorderImplementsRecorder(t *testing.T) {
	recorder := NewNoopRecorder()
	if err := recorder.CreateSubmitted(context.Background(), &tools.SubagentTaskRecord{TaskID: "subagent-1"}); err != nil {
		t.Fatalf("CreateSubmitted: %v", err)
	}
	if err := recorder.MarkRunning(context.Background(), "subagent-1", 123); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := recorder.FinishTask(context.Background(), "subagent-1", &tools.SubagentTaskFinishPatch{Status: "completed"}); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}
}
