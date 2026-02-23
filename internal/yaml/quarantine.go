package yaml

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"
)

func Quarantine(maestroDir, filePath string) error {
	quarantineDir := filepath.Join(maestroDir, "quarantine")
	if err := os.MkdirAll(quarantineDir, 0755); err != nil {
		return fmt.Errorf("create quarantine dir: %w", err)
	}

	baseName := filepath.Base(filePath)
	timestamp := time.Now().Format("20060102T150405")
	quarantineName := fmt.Sprintf("%s.%s.corrupt", baseName, timestamp)
	quarantinePath := filepath.Join(quarantineDir, quarantineName)

	if err := os.Rename(filePath, quarantinePath); err != nil {
		return fmt.Errorf("move to quarantine: %w", err)
	}

	log.Printf("quarantined corrupted file: %s → %s", filePath, quarantinePath)
	return nil
}

func RestoreFromBackup(filePath string) error {
	bakPath := filePath + ".bak"
	if _, err := os.Stat(bakPath); os.IsNotExist(err) {
		return fmt.Errorf("no backup file: %s", bakPath)
	}

	content, err := os.ReadFile(bakPath)
	if err != nil {
		return fmt.Errorf("read backup: %w", err)
	}

	// Validate backup is valid YAML
	if err := validateYAML(content); err != nil {
		return fmt.Errorf("backup YAML is also corrupted: %w", err)
	}

	if err := os.WriteFile(filePath, content, 0644); err != nil {
		return fmt.Errorf("restore from backup: %w", err)
	}

	log.Printf("restored from backup: %s → %s", bakPath, filePath)
	return nil
}

func GenerateSkeleton(filePath string, fileType string) error {
	skeleton := generateSkeletonForType(fileType)
	content, err := yamlv3.Marshal(skeleton)
	if err != nil {
		return fmt.Errorf("marshal skeleton: %w", err)
	}

	if err := os.WriteFile(filePath, content, 0644); err != nil {
		return fmt.Errorf("write skeleton: %w", err)
	}

	log.Printf("generated skeleton: %s (type: %s)", filePath, fileType)
	return nil
}

func RecoverCorruptedFile(maestroDir, filePath, fileType string) error {
	// Step 1: Quarantine the corrupted file
	if err := Quarantine(maestroDir, filePath); err != nil {
		return fmt.Errorf("quarantine failed: %w", err)
	}

	// Step 2: Try to restore from .bak
	if err := RestoreFromBackup(filePath); err != nil {
		log.Printf("backup restore failed for %s: %v — falling back to skeleton generation", filePath, err)
	} else {
		return nil
	}

	// Step 3: Generate minimal skeleton
	if err := GenerateSkeleton(filePath, fileType); err != nil {
		return fmt.Errorf("skeleton generation failed: %w", err)
	}

	return nil
}

func generateSkeletonForType(fileType string) any {
	switch fileType {
	case "queue_command":
		return map[string]any{
			"schema_version": CurrentSchemaVersion,
			"file_type":      "queue_command",
			"commands":       []any{},
		}
	case "queue_task":
		return map[string]any{
			"schema_version": CurrentSchemaVersion,
			"file_type":      "queue_task",
			"tasks":          []any{},
		}
	case "queue_notification":
		return map[string]any{
			"schema_version": CurrentSchemaVersion,
			"file_type":      "queue_notification",
			"notifications":  []any{},
		}
	case "result_task":
		return map[string]any{
			"schema_version": CurrentSchemaVersion,
			"file_type":      "result_task",
			"results":        []any{},
		}
	case "result_command":
		return map[string]any{
			"schema_version": CurrentSchemaVersion,
			"file_type":      "result_command",
			"results":        []any{},
		}
	case "state_metrics":
		return map[string]any{
			"schema_version":   CurrentSchemaVersion,
			"file_type":        "state_metrics",
			"queue_depth":      map[string]any{"planner": 0, "orchestrator": 0, "workers": map[string]any{}},
			"counters":         map[string]any{},
			"daemon_heartbeat": nil,
			"updated_at":       nil,
		}
	case "state_command":
		return map[string]any{
			"schema_version":        CurrentSchemaVersion,
			"file_type":             "state_command",
			"command_id":            "",
			"plan_version":          0,
			"plan_status":           "planning",
			"completion_policy":     map[string]any{},
			"cancel":                map[string]any{"requested": false},
			"expected_task_count":   0,
			"required_task_ids":     []any{},
			"optional_task_ids":     []any{},
			"task_dependencies":     map[string]any{},
			"task_states":           map[string]any{},
			"cancelled_reasons":     map[string]any{},
			"applied_result_ids":    map[string]any{},
			"system_commit_task_id": nil,
			"retry_lineage":         map[string]any{},
			"phases":                nil,
			"last_reconciled_at":    nil,
			"created_at":            "",
			"updated_at":            "",
		}
	case "state_continuous":
		return map[string]any{
			"schema_version":    CurrentSchemaVersion,
			"file_type":         "state_continuous",
			"current_iteration": 0,
			"max_iterations":    10,
			"status":            "stopped",
			"paused_reason":     nil,
			"last_command_id":   nil,
			"updated_at":        nil,
		}
	default:
		return map[string]any{
			"schema_version": CurrentSchemaVersion,
			"file_type":      fileType,
		}
	}
}
