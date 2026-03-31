package taskdb

import "time"

type SubagentTaskModel struct {
	ID              uint64  `gorm:"primaryKey;autoIncrement"`
	TaskID          string  `gorm:"size:64;not null;uniqueIndex:uk_subagent_tasks_task_id"`
	Source          string  `gorm:"size:32;not null;index:idx_subagent_tasks_source"`
	AgentID         *string `gorm:"size:64;index:idx_subagent_tasks_agent_id"`
	ModelName       string  `gorm:"size:128;not null"`
	Label           *string `gorm:"size:255"`
	TaskText        string  `gorm:"type:longtext;not null"`
	OriginChannel   *string `gorm:"size:64;index:idx_subagent_tasks_origin,priority:1"`
	OriginChatID    *string `gorm:"size:191;index:idx_subagent_tasks_origin,priority:2"`
	SenderID        *string `gorm:"size:500"`
	Status          string  `gorm:"size:32;not null;index:idx_subagent_tasks_status"`
	ResultText      *string `gorm:"type:longtext"`
	ErrorText       *string `gorm:"type:longtext"`
	CallbackForLLM  *string `gorm:"type:longtext"`
	CallbackForUser *string `gorm:"type:longtext"`
	Iterations      *int
	MetadataJSON    []byte `gorm:"type:json"`
	WebhookJSON     []byte `gorm:"type:json"`
	SubmittedAtMS   int64  `gorm:"not null;index:idx_subagent_tasks_submitted_at"`
	StartedAtMS     *int64
	FinishedAtMS    *int64 `gorm:"index:idx_subagent_tasks_finished_at"`
	DurationMS      *int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (SubagentTaskModel) TableName() string {
	return "subagent_tasks"
}
