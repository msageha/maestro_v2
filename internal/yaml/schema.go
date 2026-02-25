package yaml

import (
	"fmt"
	"os"

	yamlv3 "gopkg.in/yaml.v3"
)

const CurrentSchemaVersion = 1

var validFileTypes = map[string]bool{
	"queue_command":       true,
	"queue_task":          true,
	"queue_notification":  true,
	"planner_signal_queue": true,
	"result_task":         true,
	"result_command":      true,
	"state_command":       true,
	"state_metrics":       true,
	"state_continuous":    true,
}

type SchemaHeader struct {
	SchemaVersion int    `yaml:"schema_version"`
	FileType      string `yaml:"file_type"`
}

func ValidateSchemaHeader(path string, expectedFileType string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	return ValidateSchemaHeaderFromBytes(content, expectedFileType)
}

func ValidateSchemaHeaderFromBytes(content []byte, expectedFileType string) error {
	var header SchemaHeader
	if err := yamlv3.Unmarshal(content, &header); err != nil {
		return fmt.Errorf("parse yaml: %w", err)
	}

	if header.SchemaVersion < 1 {
		return fmt.Errorf("invalid schema_version %d (must be >= 1)", header.SchemaVersion)
	}
	if header.SchemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (max supported: %d)", header.SchemaVersion, CurrentSchemaVersion)
	}
	if header.FileType == "" {
		return fmt.Errorf("missing file_type")
	}
	if !validFileTypes[header.FileType] {
		return fmt.Errorf("unknown file_type: %q", header.FileType)
	}
	if expectedFileType != "" && header.FileType != expectedFileType {
		return fmt.Errorf("file_type mismatch: got %q, expected %q", header.FileType, expectedFileType)
	}

	return nil
}

func NeedsMigration(schemaVersion int) bool {
	return schemaVersion < CurrentSchemaVersion
}
