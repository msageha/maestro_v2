package plan

import "fmt"

// currentSchemaVersion is the latest schema version for state_command files.
const currentSchemaVersion = 1

// migrateFunc transforms raw YAML data from version N to version N+1.
// The map represents the unmarshalled YAML document.
type migrateFunc func(data map[string]interface{}) error

// migrator runs sequential schema migrations from an older version to the current version.
type migrator struct {
	current int
	steps   map[int]migrateFunc // key = source version (migrates from key to key+1)
}

// newMigrator creates a migrator targeting the given current version.
func newMigrator(currentVersion int) *migrator {
	return &migrator{
		current: currentVersion,
		steps:   make(map[int]migrateFunc),
	}
}

// NeedsMigration returns true if the given version is older than current.
func (m *migrator) NeedsMigration(version int) bool {
	return version > 0 && version < m.current
}

// Migrate applies all registered steps sequentially from `fromVersion` to `m.current`.
// Returns an error if any step is missing or fails.
func (m *migrator) Migrate(data map[string]interface{}, fromVersion int) error {
	if fromVersion < 1 {
		return fmt.Errorf("invalid schema version %d: must be >= 1", fromVersion)
	}
	if fromVersion > m.current {
		return fmt.Errorf("schema version %d is newer than current %d", fromVersion, m.current)
	}
	if fromVersion == m.current {
		return nil // already current
	}

	for v := fromVersion; v < m.current; v++ {
		fn, ok := m.steps[v]
		if !ok {
			return fmt.Errorf("no migration registered for version %d → %d", v, v+1)
		}
		if err := fn(data); err != nil {
			return fmt.Errorf("migration %d → %d failed: %w", v, v+1, err)
		}
		data["schema_version"] = v + 1
	}
	return nil
}

// defaultMigrator is the global migrator for state_command files.
// Additional migrations are registered here as the schema evolves.
var defaultMigrator = newMigrator(currentSchemaVersion)
