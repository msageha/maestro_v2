// Package validate provides input validation functions used throughout the
// maestro system to ensure identifiers, file paths, and names are safe for use
// in file system operations and external tools.
//
// # Main components
//
//   - ValidateID: Checks that ID strings (command_id, worker_id, task_id)
//     contain only safe characters (alphanumeric, dot, underscore, hyphen)
//     and conform to length limits.
//   - ValidateProjectName: Checks that project names are safe for use as tmux
//     session names, excluding dots and colons which have special meaning in
//     tmux target syntax.
//   - ValidateFilePath: Checks file paths for safety — non-empty, no null
//     bytes, no directory traversal ("..") — and returns the cleaned path.
//   - IsValidIdentifier: Checks that a name is a safe single directory
//     component with no path separators or null bytes.
//   - IsValidBaseName: Checks that a name is a safe single-component file
//     name containing no directory separators.
package validate
