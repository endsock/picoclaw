package taskdb

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/tools"
	"gorm.io/gorm"
)

type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

func (s *Store) CreateSubmitted(ctx context.Context, task *tools.SubagentTaskRecord) error {
	if s == nil || s.db == nil || task == nil {
		return nil
	}
	model := &SubagentTaskModel{
		TaskID:        task.TaskID,
		Source:        task.Source,
		AgentID:       stringPtr(task.AgentID),
		ModelName:     task.ModelName,
		Label:         stringPtr(task.Label),
		TaskText:      task.Task,
		OriginChannel: stringPtr(task.OriginChannel),
		OriginChatID:  stringPtr(task.OriginChatID),
		SenderID:      stringPtr(task.SenderID),
		Status:        task.Status,
		MetadataJSON:  cloneBytes(task.MetadataJSON),
		WebhookJSON:   cloneBytes(task.WebhookJSON),
		SubmittedAtMS: task.SubmittedAtMS,
	}
	return s.db.WithContext(ctx).Create(model).Error
}

func (s *Store) MarkRunning(ctx context.Context, taskID string, startedAtMS int64) error {
	if s == nil || s.db == nil || taskID == "" || startedAtMS <= 0 {
		return nil
	}
	return s.db.WithContext(ctx).
		Model(&SubagentTaskModel{}).
		Where("task_id = ? AND status = ?", taskID, "submitted").
		Updates(map[string]any{
			"status":        "running",
			"started_at_ms": startedAtMS,
		}).Error
}

func (s *Store) FinishTask(ctx context.Context, taskID string, patch *tools.SubagentTaskFinishPatch) error {
	if s == nil || s.db == nil || taskID == "" || patch == nil {
		return nil
	}
	updates := map[string]any{
		"status":            patch.Status,
		"result_text":       nullableStringValue(patch.Result),
		"error_text":        nullableStringValue(patch.Error),
		"callback_for_llm":  nullableStringValue(patch.CallbackForLLM),
		"callback_for_user": nullableStringValue(patch.CallbackForUser),
		"finished_at_ms":    patch.FinishedAtMS,
		"duration_ms":       patch.DurationMS,
	}
	if patch.Iterations > 0 {
		updates["iterations"] = patch.Iterations
	} else {
		updates["iterations"] = nil
	}
	if patch.StartedAtMS != nil {
		updates["started_at_ms"] = *patch.StartedAtMS
	}

	query := s.db.WithContext(ctx).Model(&SubagentTaskModel{}).Where("task_id = ?", taskID)
	if patch.Status == "submit_failed" {
		query = query.Where("status = ?", "submitted")
	} else {
		query = query.Where("status IN ?", []string{"submitted", "running"})
	}
	return query.Updates(updates).Error
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func stringPtr(v string) *string {
	if v == "" {
		return nil
	}
	vv := v
	return &vv
}

func nullableStringValue(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	dst := make([]byte, len(src))
	copy(dst, src)
	return dst
}
