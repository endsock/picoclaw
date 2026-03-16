package tools

import (
	"context"
	"strings"
	"testing"
)

func TestSpawnTool_Execute_EmptyTask(t *testing.T) {
	provider := &MockLLMProvider{}
	manager := NewSubagentManager(provider, "test-model", "/tmp/test", nil)
	tool := NewSpawnTool(manager)

	ctx := context.Background()

	tests := []struct {
		name string
		args map[string]any
	}{
		{"empty string", map[string]any{"task": "", "model_name": "test-model"}},
		{"whitespace only", map[string]any{"task": "   ", "model_name": "test-model"}},
		{"tabs and newlines", map[string]any{"task": "\t\n  ", "model_name": "test-model"}},
		{"missing task key", map[string]any{"label": "test", "model_name": "test-model"}},
		{"wrong type", map[string]any{"task": 123, "model_name": "test-model"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tool.Execute(ctx, tt.args)
			if result == nil {
				t.Fatal("Result should not be nil")
			}
			if !result.IsError {
				t.Error("Expected error for invalid task parameter")
			}
			if !strings.Contains(result.ForLLM, "task is required") {
				t.Errorf("Error message should mention 'task is required', got: %s", result.ForLLM)
			}
		})
	}
}

func TestSpawnTool_Execute_MissingModelName(t *testing.T) {
	provider := &MockLLMProvider{}
	manager := NewSubagentManager(provider, "test-model", "/tmp/test", nil)
	tool := NewSpawnTool(manager)

	ctx := context.Background()
	args := map[string]any{
		"task": "Write a haiku about coding",
	}

	result := tool.Execute(ctx, args)
	if result == nil {
		t.Fatal("Result should not be nil")
	}
	if !result.IsError {
		t.Error("Expected error for missing model_name parameter")
	}
	if !strings.Contains(result.ForLLM, "model_name is required") {
		t.Errorf("Error message should mention 'model_name is required', got: %s", result.ForLLM)
	}
}

func TestSpawnTool_Execute_ValidTask(t *testing.T) {
	provider := &MockLLMProvider{}
	manager := NewSubagentManager(provider, "test-model", "/tmp/test", nil)
	tool := NewSpawnTool(manager)

	ctx := context.Background()
	args := map[string]any{
		"task":       "Write a haiku about coding",
		"label":      "haiku-task",
		"model_name": "claude-sonnet--4-6",
	}

	result := tool.Execute(ctx, args)
	if result == nil {
		t.Fatal("Result should not be nil")
	}
	if result.IsError {
		t.Errorf("Expected success for valid task, got error: %s", result.ForLLM)
	}
	if !result.Async {
		t.Error("SpawnTool should return async result")
	}
}

func TestSpawnTool_Execute_NilManager(t *testing.T) {
	tool := NewSpawnTool(nil)

	ctx := context.Background()
	args := map[string]any{"task": "test task", "model_name": "test-model"}

	result := tool.Execute(ctx, args)
	if !result.IsError {
		t.Error("Expected error for nil manager")
	}
	if !strings.Contains(result.ForLLM, "Subagent manager not configured") {
		t.Errorf("Error message should mention manager not configured, got: %s", result.ForLLM)
	}
}

func TestSpawnTool_Parameters(t *testing.T) {
	tool := NewSpawnTool(nil)

	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters should not be nil")
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("Properties should be a map")
	}

	// Check required properties exist
	if _, ok := props["task"]; !ok {
		t.Error("Expected 'task' property")
	}
	if _, ok := props["label"]; !ok {
		t.Error("Expected 'label' property")
	}
	if _, ok := props["agent_id"]; !ok {
		t.Error("Expected 'agent_id' property")
	}
	if _, ok := props["model_name"]; !ok {
		t.Error("Expected 'model_name' property")
	}

	// Check required fields
	required, ok := params["required"].([]string)
	if !ok {
		t.Fatal("Required should be a string array")
	}
	hasTask, hasModel := false, false
	for _, r := range required {
		if r == "task" {
			hasTask = true
		}
		if r == "model_name" {
			hasModel = true
		}
	}
	if !hasTask {
		t.Error("Expected 'task' in required")
	}
	if !hasModel {
		t.Error("Expected 'model_name' in required")
	}
}
