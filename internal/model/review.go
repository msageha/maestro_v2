package model

import "time"

// ReviewSeverity represents the severity level of a review finding.
type ReviewSeverity string

const (
	ReviewSeverityInfo    ReviewSeverity = "info"
	ReviewSeverityWarning ReviewSeverity = "warning"
	ReviewSeverityError   ReviewSeverity = "error"
)

// ValidReviewSeverities is the set of allowed ReviewSeverity values.
var ValidReviewSeverities = map[ReviewSeverity]bool{
	ReviewSeverityInfo:    true,
	ReviewSeverityWarning: true,
	ReviewSeverityError:   true,
}

// ReviewStatus represents the lifecycle status of a review.
type ReviewStatus string

const (
	ReviewStatusPending    ReviewStatus = "pending"
	ReviewStatusInProgress ReviewStatus = "in_progress"
	ReviewStatusCompleted  ReviewStatus = "completed"
	ReviewStatusSkipped    ReviewStatus = "skipped"
)

// ValidReviewStatuses is the set of allowed ReviewStatus values.
var ValidReviewStatuses = map[ReviewStatus]bool{
	ReviewStatusPending:    true,
	ReviewStatusInProgress: true,
	ReviewStatusCompleted:  true,
	ReviewStatusSkipped:    true,
}

var terminalReviewStatuses = map[ReviewStatus]bool{
	ReviewStatusCompleted: true,
	ReviewStatusSkipped:   true,
}

// IsTerminalReviewStatus returns true if the given review status is terminal.
func IsTerminalReviewStatus(s ReviewStatus) bool {
	return terminalReviewStatuses[s]
}

// ReviewFinding represents a single finding from a code review.
type ReviewFinding struct {
	Severity     ReviewSeverity `json:"severity" yaml:"severity"`
	FilePath     string         `json:"file_path" yaml:"file_path"`
	Line         int            `json:"line" yaml:"line"`
	Message      string         `json:"message" yaml:"message"`
	SuggestedFix string         `json:"suggested_fix,omitempty" yaml:"suggested_fix,omitempty"`
}

// ReviewRequest represents a request to review code changes.
type ReviewRequest struct {
	ID            string    `json:"id" yaml:"id"`
	TaskID        string    `json:"task_id" yaml:"task_id"`
	CommandID     string    `json:"command_id" yaml:"command_id"`
	ReviewerModel string    `json:"reviewer_model" yaml:"reviewer_model"`
	FilePaths     []string  `json:"file_paths" yaml:"file_paths"`
	DiffContent   string    `json:"diff_content" yaml:"diff_content"`
	CreatedAt     time.Time `json:"created_at" yaml:"created_at"`
}

// ReviewResult represents the result of a code review.
type ReviewResult struct {
	RequestID     string          `json:"request_id" yaml:"request_id"`
	ReviewerModel string          `json:"reviewer_model" yaml:"reviewer_model"`
	Findings      []ReviewFinding `json:"findings" yaml:"findings"`
	IsAdvisory    bool            `json:"is_advisory" yaml:"is_advisory"`
	Status        ReviewStatus    `json:"status" yaml:"status"`
	Duration      time.Duration   `json:"duration" yaml:"duration"`
	CreatedAt     time.Time       `json:"created_at" yaml:"created_at"`
}

// NewReviewResult creates a new ReviewResult with IsAdvisory hardcoded to true.
// Reviews are always advisory — they provide suggestions but never block.
func NewReviewResult(requestID, reviewerModel string) *ReviewResult {
	return &ReviewResult{
		RequestID:     requestID,
		ReviewerModel: reviewerModel,
		IsAdvisory:    true,
		Status:        ReviewStatusPending,
		CreatedAt:     time.Now(),
	}
}
