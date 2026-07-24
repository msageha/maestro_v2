package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestEscapeClaudeProjectPath(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"/Users/mzk/Works/src/github.com/msageha/maestro_v2", "-Users-mzk-Works-src-github-com-msageha-maestro-v2"},
		{"/tmp/proj.dir_x", "-tmp-proj-dir-x"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := escapeClaudeProjectPath(tt.in); got != tt.want {
			t.Errorf("escapeClaudeProjectPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseEnvelope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		text        string
		wantAgent   string
		wantCommand string
		wantOK      bool
	}{
		{
			name:        "worker task envelope",
			text:        "[maestro] task_id:task-1 command_id:cmd-1 lease_epoch:1 attempt:1\n\nagent_id: worker2\npurpose: p\ncontent: c\n",
			wantAgent:   "worker2",
			wantCommand: "cmd-1",
			wantOK:      true,
		},
		{
			name:        "planner command envelope",
			text:        "[maestro] command_id:cmd-9 lease_epoch:2 attempt:1\n\ncontent: do things\n",
			wantAgent:   "planner",
			wantCommand: "cmd-9",
			wantOK:      true,
		},
		{
			name:        "planner task_result notification",
			text:        "[maestro] kind:task_result command_id:cmd-3 task_id:task-7 worker_id:worker1 status:completed\nresults/worker1.yaml を確認してください",
			wantAgent:   "planner",
			wantCommand: "cmd-3",
			wantOK:      true,
		},
		{
			name:        "orchestrator command_completed",
			text:        "[maestro] kind:command_completed command_id:cmd-3 status:completed\nresults/planner.yaml を確認してください",
			wantAgent:   "orchestrator",
			wantCommand: "cmd-3",
			wantOK:      true,
		},
		{
			name:      "orchestrator user message",
			text:      "[maestro] kind:user_message\nhello operator",
			wantAgent: "orchestrator",
			wantOK:    true,
		},
		{
			name:   "plain operator prompt",
			text:   "please fix the bug in foo.go",
			wantOK: false,
		},
		{
			name:   "task envelope without agent_id body",
			text:   "[maestro] task_id:task-1 command_id:cmd-1 lease_epoch:1 attempt:1\n\npurpose: p\n",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent, command, ok := parseEnvelope(tt.text)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if agent != tt.wantAgent || command != tt.wantCommand {
				t.Errorf("got (%q, %q), want (%q, %q)", agent, command, tt.wantAgent, tt.wantCommand)
			}
		})
	}
}

// jsonlUserLine builds a user-message record as claude-code writes it
// (content as a plain string).
func jsonlUserLine(ts, text string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":%q}}`, ts, text)
}

// jsonlAssistantLine builds an assistant record with usage.
func jsonlAssistantLine(ts, requestID, modelID string, in, out, cacheRead, cacheCreate int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":%q,"message":{"id":"msg_x","model":%q,"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":0}}}}`,
		ts, requestID, modelID, in, out, cacheRead, cacheCreate, cacheCreate)
}

const workerEnvelope = "[maestro] task_id:task-1 command_id:cmd-1 lease_epoch:1 attempt:1\n\nagent_id: worker1\npurpose: p\ncontent: c\n"

// writeSessionFixture creates <projects>/<escapedRoot>/<name>.jsonl with lines.
func writeSessionFixture(t *testing.T, projectsDir, projectRoot, name string, lines []string) string {
	t.Helper()
	dir := filepath.Join(projectsDir, escapeClaudeProjectPath(projectRoot))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestCollector(t *testing.T) (*ClaudeSessionUsageCollector, string, string) {
	t.Helper()
	projectsDir := t.TempDir()
	projectRoot := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewClaudeSessionUsageCollectorWithDir(projectsDir, projectRoot, discardLogger{})
	return c, projectsDir, projectRoot
}

func TestClaudeCollector_AttributesEnvelopeUsage(t *testing.T) {
	t.Parallel()
	c, projectsDir, projectRoot := newTestCollector(t)
	writeSessionFixture(t, projectsDir, projectRoot, "s1", []string{
		// Pre-envelope usage must not be counted.
		jsonlAssistantLine("2026-07-23T10:00:00.000Z", "req_pre", "claude-sonnet-5", 999, 999, 0, 0),
		jsonlUserLine("2026-07-23T10:00:01.000Z", workerEnvelope),
		jsonlAssistantLine("2026-07-23T10:00:02.000Z", "req_1", "claude-sonnet-5", 100, 10, 5, 7),
		jsonlAssistantLine("2026-07-23T10:00:03.000Z", "req_2", "claude-sonnet-5", 200, 20, 0, 0),
	})

	agg, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := agg.Agents["worker1"]["claude-sonnet-5"]
	want := model.TokenTotals{InputTokens: 300, OutputTokens: 30, CacheReadInputTokens: 5, CacheCreationInputTokens: 7, CacheCreation5mTokens: 7}
	if got != want {
		t.Errorf("worker1 totals = %+v, want %+v", got, want)
	}
	if cmd := agg.Commands["cmd-1"]["claude-sonnet-5"]; cmd != want {
		t.Errorf("cmd-1 totals = %+v, want %+v", cmd, want)
	}
}

func TestClaudeCollector_DedupesByRequestID(t *testing.T) {
	t.Parallel()
	c, projectsDir, projectRoot := newTestCollector(t)
	writeSessionFixture(t, projectsDir, projectRoot, "s1", []string{
		jsonlUserLine("2026-07-23T10:00:01.000Z", workerEnvelope),
		// Same requestId emitted twice (streaming re-emission).
		jsonlAssistantLine("2026-07-23T10:00:02.000Z", "req_dup", "claude-sonnet-5", 100, 10, 0, 0),
		jsonlAssistantLine("2026-07-23T10:00:02.500Z", "req_dup", "claude-sonnet-5", 100, 10, 0, 0),
	})

	agg, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := agg.Agents["worker1"]["claude-sonnet-5"]
	if got.InputTokens != 100 || got.OutputTokens != 10 {
		t.Errorf("deduped totals = %+v, want input=100 output=10", got)
	}
}

func TestClaudeCollector_IgnoresOperatorSessions(t *testing.T) {
	t.Parallel()
	c, projectsDir, projectRoot := newTestCollector(t)
	writeSessionFixture(t, projectsDir, projectRoot, "operator", []string{
		jsonlUserLine("2026-07-23T10:00:00.000Z", "fix the flaky test please"),
		jsonlAssistantLine("2026-07-23T10:00:01.000Z", "req_op", "claude-fable-5", 5000, 500, 0, 0),
	})

	agg, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(agg.Agents) != 0 || len(agg.Commands) != 0 {
		t.Errorf("operator session must not be attributed, got %+v", agg)
	}
}

func TestClaudeCollector_AttributionSwitchesOnNewEnvelope(t *testing.T) {
	t.Parallel()
	c, projectsDir, projectRoot := newTestCollector(t)
	secondEnvelope := "[maestro] task_id:task-2 command_id:cmd-2 lease_epoch:1 attempt:1\n\nagent_id: worker1\npurpose: p\ncontent: c\n"
	writeSessionFixture(t, projectsDir, projectRoot, "s1", []string{
		jsonlUserLine("2026-07-23T10:00:01.000Z", workerEnvelope),
		jsonlAssistantLine("2026-07-23T10:00:02.000Z", "req_1", "claude-sonnet-5", 100, 10, 0, 0),
		jsonlUserLine("2026-07-23T10:00:03.000Z", secondEnvelope),
		jsonlAssistantLine("2026-07-23T10:00:04.000Z", "req_2", "claude-sonnet-5", 40, 4, 0, 0),
	})

	agg, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := agg.Commands["cmd-1"]["claude-sonnet-5"].InputTokens; got != 100 {
		t.Errorf("cmd-1 input = %d, want 100", got)
	}
	if got := agg.Commands["cmd-2"]["claude-sonnet-5"].InputTokens; got != 40 {
		t.Errorf("cmd-2 input = %d, want 40", got)
	}
	if got := agg.Agents["worker1"]["claude-sonnet-5"].InputTokens; got != 140 {
		t.Errorf("worker1 input = %d, want 140", got)
	}
}

func TestClaudeCollector_SubagentAttributedByTimestamp(t *testing.T) {
	t.Parallel()
	c, projectsDir, projectRoot := newTestCollector(t)
	sessionPath := writeSessionFixture(t, projectsDir, projectRoot, "s1", []string{
		jsonlUserLine("2026-07-23T10:00:01.000Z", workerEnvelope),
		jsonlAssistantLine("2026-07-23T10:00:02.000Z", "req_1", "claude-sonnet-5", 10, 1, 0, 0),
	})
	subDir := filepath.Join(strings.TrimSuffix(sessionPath, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subLines := []string{
		// Before the envelope: dropped.
		jsonlAssistantLine("2026-07-23T09:59:00.000Z", "req_sub_early", "claude-haiku-4-5", 77, 7, 0, 0),
		// After the envelope: attributed to worker1 / cmd-1.
		jsonlAssistantLine("2026-07-23T10:00:05.000Z", "req_sub", "claude-haiku-4-5", 50, 5, 0, 0),
	}
	if err := os.WriteFile(filepath.Join(subDir, "agent-abc.jsonl"), []byte(strings.Join(subLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	agg, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := agg.Agents["worker1"]["claude-haiku-4-5"].InputTokens; got != 50 {
		t.Errorf("subagent haiku input = %d, want 50", got)
	}
	if got := agg.Commands["cmd-1"]["claude-haiku-4-5"].InputTokens; got != 50 {
		t.Errorf("cmd-1 haiku input = %d, want 50", got)
	}
}

func TestClaudeCollector_WorktreeDirsMatchedByPrefix(t *testing.T) {
	t.Parallel()
	c, projectsDir, projectRoot := newTestCollector(t)
	worktree := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd-1", "worker1")
	writeSessionFixture(t, projectsDir, worktree, "s1", []string{
		jsonlUserLine("2026-07-23T10:00:01.000Z", workerEnvelope),
		jsonlAssistantLine("2026-07-23T10:00:02.000Z", "req_1", "claude-sonnet-5", 25, 2, 0, 0),
	})

	agg, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := agg.Agents["worker1"]["claude-sonnet-5"].InputTokens; got != 25 {
		t.Errorf("worktree session input = %d, want 25", got)
	}
}

func TestClaudeCollector_CacheReusedAndInvalidated(t *testing.T) {
	t.Parallel()
	c, projectsDir, projectRoot := newTestCollector(t)
	path := writeSessionFixture(t, projectsDir, projectRoot, "s1", []string{
		jsonlUserLine("2026-07-23T10:00:01.000Z", workerEnvelope),
		jsonlAssistantLine("2026-07-23T10:00:02.000Z", "req_1", "claude-sonnet-5", 10, 1, 0, 0),
	})

	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	if len(c.cache) != 1 {
		t.Fatalf("cache size = %d, want 1", len(c.cache))
	}

	// Unchanged file: second Collect returns the same numbers via cache.
	agg, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := agg.Agents["worker1"]["claude-sonnet-5"].InputTokens; got != 10 {
		t.Errorf("cached input = %d, want 10", got)
	}

	// Grown file: signature changes, aggregate is re-parsed.
	extra := jsonlAssistantLine("2026-07-23T10:00:03.000Z", "req_2", "claude-sonnet-5", 5, 1, 0, 0)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(extra + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	agg, err = c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := agg.Agents["worker1"]["claude-sonnet-5"].InputTokens; got != 15 {
		t.Errorf("re-parsed input = %d, want 15", got)
	}

	// Deleted file: cache entry evicted.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Collect(); err != nil {
		t.Fatal(err)
	}
	if len(c.cache) != 0 {
		t.Errorf("cache size after delete = %d, want 0", len(c.cache))
	}
}

func TestClaudeCollector_MissingProjectsDir(t *testing.T) {
	t.Parallel()
	c := NewClaudeSessionUsageCollectorWithDir(filepath.Join(t.TempDir(), "nope"), "/some/root", discardLogger{})
	agg, err := c.Collect()
	if err != nil {
		t.Fatalf("missing projects dir must not error, got %v", err)
	}
	if agg != nil {
		t.Errorf("agg = %+v, want nil for absent source", agg)
	}
}

func TestClaudeCollector_ContentBlockArrayForm(t *testing.T) {
	t.Parallel()
	c, projectsDir, projectRoot := newTestCollector(t)
	// User content as an array of text blocks instead of a plain string.
	envelope := strings.ReplaceAll(workerEnvelope, "\n", "\\n")
	userLine := fmt.Sprintf(`{"type":"user","timestamp":"2026-07-23T10:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"%s"}]}}`, envelope)
	writeSessionFixture(t, projectsDir, projectRoot, "s1", []string{
		userLine,
		jsonlAssistantLine("2026-07-23T10:00:02.000Z", "req_1", "claude-sonnet-5", 30, 3, 0, 0),
	})

	agg, err := c.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := agg.Agents["worker1"]["claude-sonnet-5"].InputTokens; got != 30 {
		t.Errorf("block-array envelope input = %d, want 30", got)
	}
}
