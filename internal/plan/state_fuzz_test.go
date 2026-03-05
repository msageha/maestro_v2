package plan

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	yamlv3 "gopkg.in/yaml.v3"
)

func FuzzLoadStateYAML(f *testing.F) {
	// Valid minimal state
	f.Add([]byte(`schema_version: 1
file_type: state_command
command_id: cmd_001
plan_status: sealed
expected_task_count: 0
`))

	// Valid state with tasks
	f.Add([]byte(`schema_version: 1
file_type: state_command
command_id: cmd_002
plan_status: sealed
expected_task_count: 1
required_task_ids: [task_1]
task_states:
  task_1: completed
`))

	// Empty document
	f.Add([]byte(""))

	// Malformed YAML
	f.Add([]byte(":\n"))
	f.Add([]byte("{{{"))
	f.Add([]byte("schema_version: notanumber\n"))

	// Wrong file_type
	f.Add([]byte("schema_version: 1\nfile_type: wrong\n"))

	// Future schema version
	f.Add([]byte("schema_version: 999\nfile_type: state_command\n"))

	// Deeply nested YAML
	f.Add([]byte("a:\n  b:\n    c:\n      d: 1\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}

		// The fuzz target verifies that parsing never panics.
		var state model.CommandState
		if err := yamlv3.Unmarshal(data, &state); err != nil {
			return
		}

		// If parsing succeeds, validate schema constraints the same way
		// loadAndParseState does. These should not panic.
		if state.SchemaVersion > CurrentSchemaVersion {
			return
		}
		if state.SchemaVersion < 1 {
			return
		}
		if state.FileType != "state_command" {
			return
		}

		// If we get here, it's a valid-looking state. Verify roundtrip
		// marshalling doesn't panic.
		_, _ = yamlv3.Marshal(&state)
	})
}
