package taskdb

import (
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/tools"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func NewRecorder(cfg config.TaskDBConfig) (tools.SubagentTaskRecorder, error) {
	if !cfg.Enabled {
		return NewNoopRecorder(), nil
	}
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, fmt.Errorf("task_db.dsn is required when task_db.enabled is true")
	}
	db, err := gorm.Open(mysql.Open(cfg.DSN), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open task db: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get task db sql handle: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetimeMinutes > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetimeMinutes) * time.Minute)
	}
	if cfg.AutoMigrate {
		if err := db.AutoMigrate(&SubagentTaskModel{}); err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("auto migrate task db: %w", err)
		}
	}
	return NewStore(db), nil
}
