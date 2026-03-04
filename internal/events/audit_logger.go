package events

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// Default maximum log file size (100MB)
	DefaultMaxLogSize = 100 * 1024 * 1024
	// Log file extension
	LogFileExtension = ".jsonl"
	// Archive directory name
	ArchiveDir = "archive"
)

// LogEntry represents a single audit log entry
type LogEntry struct {
	Timestamp   time.Time              `json:"timestamp"`
	EventType   string                 `json:"event_type"`
	EventID     string                 `json:"event_id,omitempty"`
	CommandID   string                 `json:"command_id,omitempty"`
	TaskID      string                 `json:"task_id,omitempty"`
	AgentID     string                 `json:"agent_id,omitempty"`
	Details     map[string]interface{} `json:"details,omitempty"`
	Checksum    string                 `json:"checksum,omitempty"`
}

// AuditLogger provides append-only logging functionality with rotation
type AuditLogger struct {
	mu              sync.Mutex
	file            *os.File
	currentSize     int64
	maxSize         int64
	logPath         string
	enableChecksum  bool
	rotationCounter int
}

// NewAuditLogger creates a new audit logger instance
func NewAuditLogger(logPath string, maxSize int64) (*AuditLogger, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxLogSize
	}

	logger := &AuditLogger{
		logPath: logPath,
		maxSize: maxSize,
	}

	// Ensure log directory exists
	logDir := filepath.Dir(logPath)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	// Open or create log file
	if err := logger.openLogFile(); err != nil {
		return nil, err
	}

	return logger, nil
}

// openLogFile opens the log file and gets its current size
func (l *AuditLogger) openLogFile() error {
	file, err := os.OpenFile(l.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	stat, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("failed to stat log file: %w", err)
	}

	l.file = file
	l.currentSize = stat.Size()
	return nil
}

// Log writes a log entry to the audit log
func (l *AuditLogger) Log(eventType string, details map[string]interface{}) error {
	entry := LogEntry{
		Timestamp: time.Now().UTC(),
		EventType: eventType,
		Details:   details,
	}

	// Extract common fields from details if present
	if eventID, ok := details["event_id"].(string); ok {
		entry.EventID = eventID
	}
	if commandID, ok := details["command_id"].(string); ok {
		entry.CommandID = commandID
	}
	if taskID, ok := details["task_id"].(string); ok {
		entry.TaskID = taskID
	}
	if agentID, ok := details["agent_id"].(string); ok {
		entry.AgentID = agentID
	}

	return l.WriteEntry(&entry)
}

// WriteEntry writes a structured log entry to the file
func (l *AuditLogger) WriteEntry(entry *LogEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Add checksum if enabled
	if l.enableChecksum {
		entry.Checksum = l.calculateChecksum(entry)
	}

	// Marshal entry to JSON
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("failed to marshal log entry: %w", err)
	}

	// Add newline for JSONL format
	data = append(data, '\n')

	// Check if rotation is needed
	if l.currentSize+int64(len(data)) > l.maxSize {
		if err := l.rotate(); err != nil {
			return fmt.Errorf("failed to rotate log: %w", err)
		}
	}

	// Write to file with lock
	n, err := l.file.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write log entry: %w", err)
	}

	// Sync to disk for durability
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("failed to sync log file: %w", err)
	}

	l.currentSize += int64(n)
	return nil
}

// rotate performs log rotation
func (l *AuditLogger) rotate() error {
	// Close current file
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("failed to close current log file: %w", err)
	}

	// Create archive directory if needed
	archiveDir := filepath.Join(filepath.Dir(l.logPath), ArchiveDir)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		// Re-open original to keep logger usable
		if reopenErr := l.openLogFile(); reopenErr != nil {
			return fmt.Errorf("failed to create archive directory: %v; failed to reopen log: %w", err, reopenErr)
		}
		return fmt.Errorf("failed to create archive directory: %w", err)
	}

	// Generate archive filename with timestamp
	timestamp := time.Now().Format("20060102_150405")
	l.rotationCounter++
	baseName := filepath.Base(l.logPath)
	archiveName := fmt.Sprintf("%s.%s.%d%s",
		baseName[:len(baseName)-len(LogFileExtension)],
		timestamp,
		l.rotationCounter,
		LogFileExtension)
	archivePath := filepath.Join(archiveDir, archiveName)

	// Move current log to archive
	if err := os.Rename(l.logPath, archivePath); err != nil {
		// Re-open original to keep logger usable
		if reopenErr := l.openLogFile(); reopenErr != nil {
			return fmt.Errorf("failed to archive log file: %v; failed to reopen log: %w", err, reopenErr)
		}
		return fmt.Errorf("failed to archive log file: %w", err)
	}

	// Open new log file
	if err := l.openLogFile(); err != nil {
		// Rename succeeded but new file open failed — roll back archive
		if rollbackErr := os.Rename(archivePath, l.logPath); rollbackErr == nil {
			// Rollback succeeded, try to reopen original
			if reopenErr := l.openLogFile(); reopenErr != nil {
				return fmt.Errorf("failed to open new log file: %v; rollback succeeded but reopen failed: %w", err, reopenErr)
			}
			return fmt.Errorf("failed to open new log file (rolled back): %w", err)
		}
		return fmt.Errorf("failed to open new log file after rotation: %w", err)
	}

	return nil
}

// checksumSHA256Hex computes a SHA-256 checksum and returns hex-encoded string
func checksumSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// checksumDJB2Legacy computes the legacy djb2 hash for backward compatibility (read-only)
func checksumDJB2Legacy(data []byte) string {
	var h uint64 = 5381
	for _, b := range data {
		h = ((h << 5) + h) + uint64(b)
	}
	return fmt.Sprintf("%x", h)
}

const checksumPrefixSHA256 = "sha256:"

// calculateChecksum calculates a SHA-256 checksum for integrity verification
func (l *AuditLogger) calculateChecksum(entry *LogEntry) string {
	// Create a copy without the checksum field
	entryCopy := *entry
	entryCopy.Checksum = ""

	data, err := json.Marshal(entryCopy)
	if err != nil {
		return ""
	}

	return checksumPrefixSHA256 + checksumSHA256Hex(data)
}

// verifyChecksum verifies a checksum against data, supporting both SHA-256 and legacy djb2 formats
func verifyChecksum(expected string, data []byte) bool {
	switch {
	case strings.HasPrefix(expected, checksumPrefixSHA256):
		return strings.TrimPrefix(expected, checksumPrefixSHA256) == checksumSHA256Hex(data)
	default:
		// Legacy unprefixed djb2 checksum
		return expected == checksumDJB2Legacy(data)
	}
}

// EnableChecksum enables checksum calculation for log entries
func (l *AuditLogger) EnableChecksum(enable bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enableChecksum = enable
}

// IntegrityResult contains detailed results of a log integrity verification
type IntegrityResult struct {
	TotalEntries           int
	ValidEntries           int
	MalformedEntries       int
	InvalidChecksumEntries int
}

// VerifyLogIntegrityDetailed verifies the integrity of log entries and returns detailed results
func VerifyLogIntegrityDetailed(logPath string) (IntegrityResult, error) {
	file, err := os.Open(logPath)
	if err != nil {
		return IntegrityResult{}, fmt.Errorf("failed to open log file: %w", err)
	}
	defer file.Close()

	var result IntegrityResult
	scanner := bufio.NewScanner(file)
	// Increase buffer size for potentially large log lines
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		// Skip empty lines
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		result.TotalEntries++

		var entry LogEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			result.MalformedEntries++
			continue
		}

		// If entry has checksum, verify it
		if entry.Checksum != "" {
			expectedChecksum := entry.Checksum
			entry.Checksum = ""

			data, err := json.Marshal(entry)
			if err != nil {
				result.InvalidChecksumEntries++
				continue
			}

			if verifyChecksum(expectedChecksum, data) {
				result.ValidEntries++
			} else {
				result.InvalidChecksumEntries++
			}
		} else {
			// Entries without checksum are considered valid
			result.ValidEntries++
		}
	}

	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("error reading log file: %w", err)
	}

	return result, nil
}

// VerifyLogIntegrity verifies the integrity of log entries in a file.
// Returns total entries, valid entries, and an error if malformed or invalid entries are found.
func VerifyLogIntegrity(logPath string) (int, int, error) {
	result, err := VerifyLogIntegrityDetailed(logPath)
	if err != nil {
		return result.TotalEntries, result.ValidEntries, err
	}

	if result.MalformedEntries > 0 || result.InvalidChecksumEntries > 0 {
		return result.TotalEntries, result.ValidEntries, fmt.Errorf(
			"integrity check found issues: %d malformed entries, %d invalid checksums out of %d total entries",
			result.MalformedEntries, result.InvalidChecksumEntries, result.TotalEntries,
		)
	}

	return result.TotalEntries, result.ValidEntries, nil
}

// Close closes the audit logger
func (l *AuditLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		if err := l.file.Sync(); err != nil {
			return err
		}
		return l.file.Close()
	}
	return nil
}

// GetCurrentLogPath returns the current log file path
func (l *AuditLogger) GetCurrentLogPath() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logPath
}

// GetCurrentSize returns the current size of the log file
func (l *AuditLogger) GetCurrentSize() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.currentSize
}