package plan

import "fmt"

// CurrentSchemaVersion is the latest schema version for state_command files.
const CurrentSchemaVersion = 1

// MigrateFunc transforms raw YAML data from version N to version N+1.
// The map represents the unmarshalled YAML document.
type MigrateFunc func(data map[string]interface{}) error

// Migrator runs sequential schema migrations from an older version to the current version.
type Migrator struct {
	current int
	steps   map[int]MigrateFunc // key = source version (migrates from key to key+1)
}

// NewMigrator creates a Migrator targeting the given current version.
func NewMigrator(currentVersion int) *Migrator {
	return &Migrator{
		current: currentVersion,
		steps:   make(map[int]MigrateFunc),
	}
}

// Register adds a migration step from version `from` to `from+1`.
// Panics if a step for that version is already registered.
func (m *Migrator) Register(from int, fn MigrateFunc) {
	if _, exists := m.steps[from]; exists {
		panic(fmt.Sprintf("migration step from version %d already registered", from))
	}
	m.steps[from] = fn
}

// NeedsMigration returns true if the given version is older than current.
func (m *Migrator) NeedsMigration(version int) bool {
	return version > 0 && version < m.current
}

// Migrate applies all registered steps sequentially from `fromVersion` to `m.current`.
// Returns an error if any step is missing or fails.
func (m *Migrator) Migrate(data map[string]interface{}, fromVersion int) error {
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

// DefaultMigrator is the global migrator for state_command files.
// Additional migrations are registered here as the schema evolves.
var DefaultMigrator = NewMigrator(CurrentSchemaVersion)
