package model

import "time"

// ReviewSeverity represents the severity level of a review finding.
type ReviewSeverity string

const (
	// ReviewSeverityInfo is the lowest review severity, used for informational findings.
	ReviewSeverityInfo ReviewSeverity = "info"
	// ReviewSeverityWarning indicates a finding that should be addressed but is not blocking.
	ReviewSeverityWarning ReviewSeverity = "warning"
	// ReviewSeverityError indicates a blocking finding that must be resolved.
	ReviewSeverityError ReviewSeverity = "error"
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
	// ReviewStatusPending indicates the review has not yet started.
	ReviewStatusPending ReviewStatus = "pending"
	// ReviewStatusInProgress indicates the review is currently being conducted.
	ReviewStatusInProgress ReviewStatus = "in_progress"
	// ReviewStatusCompleted indicates the review finished successfully.
	ReviewStatusCompleted ReviewStatus = "completed"
	// ReviewStatusSkipped indicates the review was bypassed.
	ReviewStatusSkipped ReviewStatus = "skipped"
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
	// SkipReason is populated only when Status==ReviewStatusSkipped to
	// explain why the review never executed (empty diff, context cancel,
	// invocation error, parse failure, etc.). Mirrored into the per-task
	// review audit YAML so an operator can see at a glance why a
	// particular reviewer model returned 0 findings, instead of having
	// to grep daemon.log for the matching review_id.
	SkipReason string        `json:"skip_reason,omitempty" yaml:"skip_reason,omitempty"`
	Duration   time.Duration `json:"duration" yaml:"duration"`
	CreatedAt  time.Time     `json:"created_at" yaml:"created_at"`
}

// NewReviewResult creates a new ReviewResult with the given advisory flag.
// When isAdvisory is true, the review provides suggestions but never blocks.
func NewReviewResult(requestID, reviewerModel string, isAdvisory bool) *ReviewResult {
	return &ReviewResult{
		RequestID:     requestID,
		ReviewerModel: reviewerModel,
		IsAdvisory:    isAdvisory,
		Status:        ReviewStatusPending,
		CreatedAt:     time.Now(),
	}
}
