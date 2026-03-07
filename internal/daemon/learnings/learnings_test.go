package learnings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	yamlv3 "gopkg.in/yaml.v3"
)

func writeLearningsFile(t *testing.T, dir string, lf model.LearningsFile) {
	t.Helper()
	learningsPath := filepath.Join(dir, "state", "learnings.yaml")
	if err := os.MkdirAll(filepath.Dir(learningsPath), 0755); err != nil {
		t.Fatal(err)
	}
	data, err := yamlv3.Marshal(lf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(learningsPath, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestReadTopKLearnings_NoFile(t *testing.T) {
	dir := t.TempDir()
	cfg := model.LearningsConfig{InjectCount: 5, TTLHours: 72}
	result, err := ReadTopKLearnings(dir, cfg, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestReadTopKLearnings_EmptyLearnings(t *testing.T) {
	dir := t.TempDir()
	writeLearningsFile(t, dir, model.LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings:     []model.Learning{},
	})
	cfg := model.LearningsConfig{InjectCount: 5, TTLHours: 72}
	result, err := ReadTopKLearnings(dir, cfg, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestReadTopKLearnings_TopK(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	learnings := make([]model.Learning, 10)
	for i := range learnings {
		learnings[i] = model.Learning{
			ResultID:  "r" + string(rune('0'+i)),
			CommandID: "cmd1",
			Content:   "learning " + string(rune('0'+i)),
			CreatedAt: now.Add(-time.Duration(10-i) * time.Hour).Format(time.RFC3339),
		}
	}
	writeLearningsFile(t, dir, model.LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings:     learnings,
	})

	cfg := model.LearningsConfig{InjectCount: 3, TTLHours: 72}
	result, err := ReadTopKLearnings(dir, cfg, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 learnings, got %d", len(result))
	}
	// Should be the last 3 (most recent)
	if result[0].Content != "learning 7" {
		t.Errorf("expected 'learning 7', got %q", result[0].Content)
	}
	if result[2].Content != "learning 9" {
		t.Errorf("expected 'learning 9', got %q", result[2].Content)
	}
}

func TestReadTopKLearnings_TTLFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	learnings := []model.Learning{
		{ResultID: "r1", Content: "old learning", CreatedAt: now.Add(-100 * time.Hour).Format(time.RFC3339)},
		{ResultID: "r2", Content: "recent learning", CreatedAt: now.Add(-1 * time.Hour).Format(time.RFC3339)},
	}
	writeLearningsFile(t, dir, model.LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings:     learnings,
	})

	cfg := model.LearningsConfig{InjectCount: 5, TTLHours: 72}
	result, err := ReadTopKLearnings(dir, cfg, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 learning after TTL filter, got %d", len(result))
	}
	if result[0].Content != "recent learning" {
		t.Errorf("expected 'recent learning', got %q", result[0].Content)
	}
}

func TestReadTopKLearnings_TTLZeroUnlimited(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	learnings := []model.Learning{
		{ResultID: "r1", Content: "very old", CreatedAt: now.Add(-10000 * time.Hour).Format(time.RFC3339)},
		{ResultID: "r2", Content: "recent", CreatedAt: now.Add(-1 * time.Hour).Format(time.RFC3339)},
	}
	writeLearningsFile(t, dir, model.LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings:     learnings,
	})

	// TTLHours: 0 = unlimited (no expiry)
	cfg := model.LearningsConfig{InjectCount: 5, TTLHours: 0}
	result, err := ReadTopKLearnings(dir, cfg, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 learnings with TTL=0 (unlimited), got %d", len(result))
	}
}

func TestReadTopKLearnings_AllExpired(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	learnings := []model.Learning{
		{ResultID: "r1", Content: "old1", CreatedAt: now.Add(-200 * time.Hour).Format(time.RFC3339)},
		{ResultID: "r2", Content: "old2", CreatedAt: now.Add(-100 * time.Hour).Format(time.RFC3339)},
	}
	writeLearningsFile(t, dir, model.LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings:     learnings,
	})

	cfg := model.LearningsConfig{InjectCount: 5, TTLHours: 72}
	result, err := ReadTopKLearnings(dir, cfg, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil when all expired, got %v", result)
	}
}

func TestReadTopKLearnings_MalformedTimestamp(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	learnings := []model.Learning{
		{ResultID: "r1", Content: "bad timestamp", CreatedAt: "not-a-date"},
		{ResultID: "r2", Content: "good", CreatedAt: now.Add(-1 * time.Hour).Format(time.RFC3339)},
	}
	writeLearningsFile(t, dir, model.LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings:     learnings,
	})

	cfg := model.LearningsConfig{InjectCount: 5, TTLHours: 72}
	result, err := ReadTopKLearnings(dir, cfg, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 (malformed skipped), got %d", len(result))
	}
	if result[0].Content != "good" {
		t.Errorf("expected 'good', got %q", result[0].Content)
	}
}

func TestReadTopKLearnings_CorruptYAML(t *testing.T) {
	dir := t.TempDir()
	learningsPath := filepath.Join(dir, "state", "learnings.yaml")
	if err := os.MkdirAll(filepath.Dir(learningsPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(learningsPath, []byte("{{{{invalid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := model.LearningsConfig{InjectCount: 5, TTLHours: 72}
	result, err := ReadTopKLearnings(dir, cfg, time.Now())
	if err == nil {
		t.Fatal("corrupt YAML should return error")
	}
	if result != nil {
		t.Fatalf("corrupt YAML should return nil result, got %v", result)
	}
}

func TestFormatLearningsSection_Empty(t *testing.T) {
	result := FormatLearningsSection(nil)
	if result != "" {
		t.Fatalf("expected empty string for nil learnings, got %q", result)
	}

	result = FormatLearningsSection([]model.Learning{})
	if result != "" {
		t.Fatalf("expected empty string for empty learnings, got %q", result)
	}
}

func TestFormatLearningsSection_Format(t *testing.T) {
	learnings := []model.Learning{
		{Content: "Always run tests before commit"},
		{Content: "Use gofmt for formatting"},
	}
	result := FormatLearningsSection(learnings)

	if !strings.Contains(result, "参考: 過去の学習知見") {
		t.Error("missing section header")
	}
	if !strings.Contains(result, "- Always run tests before commit") {
		t.Error("missing first learning")
	}
	if !strings.Contains(result, "- Use gofmt for formatting") {
		t.Error("missing second learning")
	}
	if !strings.HasPrefix(result, "\n\n---\n") {
		t.Error("missing separator prefix")
	}
}

func TestReadTopKLearnings_FewerThanK(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	learnings := []model.Learning{
		{ResultID: "r1", Content: "only one", CreatedAt: now.Add(-1 * time.Hour).Format(time.RFC3339)},
	}
	writeLearningsFile(t, dir, model.LearningsFile{
		SchemaVersion: 1,
		FileType:      "state_learnings",
		Learnings:     learnings,
	})

	cfg := model.LearningsConfig{InjectCount: 5, TTLHours: 72}
	result, err := ReadTopKLearnings(dir, cfg, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 learning, got %d", len(result))
	}
}

func TestEffectiveInjectCount_Defaults(t *testing.T) {
	cfg := model.LearningsConfig{}
	if cfg.EffectiveInjectCount() != 5 {
		t.Errorf("expected default inject_count=5, got %d", cfg.EffectiveInjectCount())
	}

	cfg = model.LearningsConfig{InjectCount: 10}
	if cfg.EffectiveInjectCount() != 10 {
		t.Errorf("expected inject_count=10, got %d", cfg.EffectiveInjectCount())
	}
}

func TestEffectiveTTLHours(t *testing.T) {
	cfg := model.LearningsConfig{TTLHours: 0}
	if cfg.EffectiveTTLHours() != 0 {
		t.Errorf("expected ttl_hours=0 (unlimited), got %d", cfg.EffectiveTTLHours())
	}

	cfg = model.LearningsConfig{TTLHours: 48}
	if cfg.EffectiveTTLHours() != 48 {
		t.Errorf("expected ttl_hours=48, got %d", cfg.EffectiveTTLHours())
	}
}
