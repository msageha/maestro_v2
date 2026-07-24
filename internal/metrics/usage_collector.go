//revive:disable-next-line:var-naming // package name is intentional; see types.go
package metrics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// UsageAggregate is the raw result of a collection pass: token totals keyed
// by agent ID and by command ID, each broken down per model so cost can be
// (re)derived from a price table.
type UsageAggregate struct {
	Agents   map[string]map[string]model.TokenTotals // agentID -> modelID -> totals
	Commands map[string]map[string]model.TokenTotals // commandID -> modelID -> totals
}

func newUsageAggregate() *UsageAggregate {
	return &UsageAggregate{
		Agents:   make(map[string]map[string]model.TokenTotals),
		Commands: make(map[string]map[string]model.TokenTotals),
	}
}

func addTokens(m map[string]map[string]model.TokenTotals, key, modelID string, t model.TokenTotals) {
	if key == "" {
		return
	}
	inner, ok := m[key]
	if !ok {
		inner = make(map[string]model.TokenTotals)
		m[key] = inner
	}
	cur := inner[modelID]
	cur.Add(t)
	inner[modelID] = cur
}

// merge folds other into a.
func (a *UsageAggregate) merge(other *UsageAggregate) {
	for agent, byModel := range other.Agents {
		for modelID, t := range byModel {
			addTokens(a.Agents, agent, modelID, t)
		}
	}
	for cmd, byModel := range other.Commands {
		for modelID, t := range byModel {
			addTokens(a.Commands, cmd, modelID, t)
		}
	}
}

// UsageCollector produces a UsageAggregate from a runtime-specific local
// source. Implementations must be read-only and side-effect free.
type UsageCollector interface {
	// Collect returns the aggregate over currently retained session data.
	// A nil aggregate with nil error means "source not present" (e.g. no
	// ~/.claude directory) and is treated as an empty result.
	Collect() (*UsageAggregate, error)
	// Source is a short human-readable description of the collection path.
	Source() string
}

// ClaudeSessionUsageCollector collects token usage from claude-code's local
// session records.
//
// claude-code appends one JSONL record per event to
// "<claudeConfigDir>/projects/<escaped-cwd>/<sessionID>.jsonl", where
// <escaped-cwd> is the session's initial working directory with every
// non-alphanumeric byte replaced by '-'. Assistant records carry
// message.usage token counters (deduplicated by requestId across streaming
// re-emissions); user records carry the prompt text verbatim.
//
// Maestro delivers work through tmux send-keys using envelopes that begin
// with "[maestro] " (internal/envelope). Those envelopes therefore appear as
// user records in the session file and are the attribution anchor:
//
//	[maestro] task_id:T command_id:C ...  + "agent_id: workerN" body line
//	    -> agent=workerN, command=C            (worker task dispatch)
//	[maestro] command_id:C ...                (no task_id / kind)
//	    -> agent=planner, command=C            (planner command dispatch)
//	[maestro] kind:task_result command_id:C ...
//	    -> agent=planner, command=C            (planner side notification)
//	[maestro] kind:command_completed|command_failed|command_cancelled command_id:C
//	    -> agent=orchestrator, command=C
//	[maestro] kind:user_message
//	    -> agent=orchestrator
//
// Usage between two envelopes is attributed to the most recent envelope
// (worker panes are /clear-ed per dispatch, so in practice one session maps
// to one task). Usage that precedes the first envelope of a session — and
// any session with no envelope at all (operator sessions in the same
// project) — is intentionally not counted. Subagent transcripts under
// <sessionID>/subagents/*.jsonl are attributed by timestamp against the
// parent session's envelope timeline. This is a best-effort approximation:
// per-task boundaries are as precise as the envelope timeline, not exact
// API-side accounting.
type ClaudeSessionUsageCollector struct {
	projectsDir  string   // <claude config dir>/projects
	projectRoots []string // logical + symlink-resolved project root
	logger       Logger
	cache        map[string]*sessionCacheEntry // keyed by session file path
}

// sessionCacheEntry caches the parsed aggregate of one session (main file +
// its subagent transcripts) keyed by a cheap change signature.
type sessionCacheEntry struct {
	signature string
	aggregate *UsageAggregate
}

// NewClaudeSessionUsageCollector builds a collector for the given project
// root, reading from the default claude config dir ($CLAUDE_CONFIG_DIR or
// ~/.claude).
func NewClaudeSessionUsageCollector(projectRoot string, logger Logger) *ClaudeSessionUsageCollector {
	return NewClaudeSessionUsageCollectorWithDir(defaultClaudeProjectsDir(), projectRoot, logger)
}

// NewClaudeSessionUsageCollectorWithDir is the injectable-constructor variant
// used by tests and by callers with a non-default claude config dir.
func NewClaudeSessionUsageCollectorWithDir(projectsDir, projectRoot string, logger Logger) *ClaudeSessionUsageCollector {
	roots := []string{projectRoot}
	if resolved, err := filepath.EvalSymlinks(projectRoot); err == nil && resolved != projectRoot {
		roots = append(roots, resolved)
	}
	return &ClaudeSessionUsageCollector{
		projectsDir:  projectsDir,
		projectRoots: roots,
		logger:       logger,
		cache:        make(map[string]*sessionCacheEntry),
	}
}

// Source implements UsageCollector.
func (c *ClaudeSessionUsageCollector) Source() string {
	return "claude-code session files (" + c.projectsDir + ")"
}

func defaultClaudeProjectsDir() string {
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		base = filepath.Join(home, ".claude")
	}
	return filepath.Join(base, "projects")
}

// escapeClaudeProjectPath mirrors claude-code's project-directory naming:
// every byte outside [A-Za-z0-9] becomes '-'.
var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

func escapeClaudeProjectPath(p string) string {
	return nonAlnum.ReplaceAllString(p, "-")
}

// Collect implements UsageCollector.
func (c *ClaudeSessionUsageCollector) Collect() (*UsageAggregate, error) {
	if c.projectsDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(c.projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read claude projects dir: %w", err)
	}

	prefixes := make([]string, 0, len(c.projectRoots))
	for _, root := range c.projectRoots {
		prefixes = append(prefixes, escapeClaudeProjectPath(root))
	}

	agg := newUsageAggregate()
	liveSessions := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() || !c.matchesProject(entry.Name(), prefixes) {
			continue
		}
		dir := filepath.Join(c.projectsDir, entry.Name())
		sessions, err := os.ReadDir(dir)
		if err != nil {
			c.logger.Warnf("usage_collect read project dir %s: %v", dir, err)
			continue
		}
		for _, s := range sessions {
			if s.IsDir() || !strings.HasSuffix(s.Name(), ".jsonl") {
				continue
			}
			sessionPath := filepath.Join(dir, s.Name())
			liveSessions[sessionPath] = struct{}{}
			sessionAgg := c.sessionAggregate(sessionPath)
			if sessionAgg != nil {
				agg.merge(sessionAgg)
			}
		}
	}

	// Evict cache entries for sessions that disappeared (retention pruning).
	for path := range c.cache {
		if _, ok := liveSessions[path]; !ok {
			delete(c.cache, path)
		}
	}
	return agg, nil
}

// matchesProject reports whether a projects/ subdirectory belongs to this
// project root: exact escaped match, or escaped-prefix + "-" (worktrees and
// other subdirectories of the project root, e.g. .maestro/worktrees/...).
// Envelope gating downstream filters any false positives this admits.
func (c *ClaudeSessionUsageCollector) matchesProject(dirName string, prefixes []string) bool {
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		if dirName == p || strings.HasPrefix(dirName, p+"-") {
			return true
		}
	}
	return false
}

// sessionAggregate returns the (possibly cached) aggregate for one session.
// Errors are logged and degrade to a nil aggregate — collection must never
// take the daemon down.
func (c *ClaudeSessionUsageCollector) sessionAggregate(sessionPath string) *UsageAggregate {
	subagentDir := filepath.Join(strings.TrimSuffix(sessionPath, ".jsonl"), "subagents")
	sig, err := sessionSignature(sessionPath, subagentDir)
	if err != nil {
		c.logger.Warnf("usage_collect stat session %s: %v", sessionPath, err)
		return nil
	}
	if cached, ok := c.cache[sessionPath]; ok && cached.signature == sig {
		return cached.aggregate
	}

	agg, err := c.parseSession(sessionPath, subagentDir)
	if err != nil {
		c.logger.Warnf("usage_collect parse session %s: %v", sessionPath, err)
		// Cache the partial result anyway: a permanently unparseable file
		// should not be re-read on every scan.
	}
	c.cache[sessionPath] = &sessionCacheEntry{signature: sig, aggregate: agg}
	return agg
}

// sessionSignature builds a cheap change signature over the main session
// file and every subagent transcript (path, size, mtime).
func sessionSignature(sessionPath, subagentDir string) (string, error) {
	info, err := os.Stat(sessionPath)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d:%d", info.Size(), info.ModTime().UnixNano())
	subEntries, err := os.ReadDir(subagentDir)
	if err != nil {
		// Missing subagent dir is the common case.
		return sb.String(), nil
	}
	names := make([]string, 0, len(subEntries))
	for _, e := range subEntries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if fi, err := os.Stat(filepath.Join(subagentDir, name)); err == nil {
			fmt.Fprintf(&sb, "|%s:%d:%d", name, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return sb.String(), nil
}

// --- session JSONL parsing ---

type sessionUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u *sessionUsage) totals() model.TokenTotals {
	t := model.TokenTotals{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
	}
	if u.CacheCreation != nil {
		t.CacheCreation5mTokens = u.CacheCreation.Ephemeral5m
		t.CacheCreation1hTokens = u.CacheCreation.Ephemeral1h
	}
	return t
}

type sessionLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	RequestID string `json:"requestId"`
	Message   *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *sessionUsage   `json:"usage"`
	} `json:"message"`
}

// attribution is one envelope-derived (agent, command) assignment, valid
// from At until the next envelope.
type attribution struct {
	At      time.Time
	Agent   string
	Command string
}

var (
	taskHeaderRe    = regexp.MustCompile(`^\[maestro\] task_id:(\S+) command_id:(\S+)`)
	plannerHeaderRe = regexp.MustCompile(`^\[maestro\] command_id:(\S+)`)
	kindHeaderRe    = regexp.MustCompile(`^\[maestro\] kind:(\S+)`)
	commandFieldRe  = regexp.MustCompile(`\bcommand_id:(\S+)`)
	agentIDLineRe   = regexp.MustCompile(`(?m)^agent_id: (\S+)`)
)

// parseEnvelope extracts an attribution from a user-message text if it is a
// maestro delivery envelope; ok=false otherwise.
func parseEnvelope(text string) (agent, command string, ok bool) {
	if !strings.HasPrefix(text, "[maestro] ") {
		return "", "", false
	}
	if m := taskHeaderRe.FindStringSubmatch(text); m != nil {
		agent = ""
		if am := agentIDLineRe.FindStringSubmatch(text); am != nil {
			agent = am[1]
		}
		return agent, m[2], agent != ""
	}
	if m := kindHeaderRe.FindStringSubmatch(text); m != nil {
		command = ""
		if cm := commandFieldRe.FindStringSubmatch(text); cm != nil {
			command = cm[1]
		}
		switch {
		case m[1] == "task_result":
			return "planner", command, true
		case strings.HasPrefix(m[1], "command_"):
			return "orchestrator", command, true
		case m[1] == string(model.NotificationTypeUserMessage):
			return "orchestrator", "", true
		default:
			// Unknown kind: keep it attributable to the orchestrator
			// notification channel rather than dropping the usage.
			return "orchestrator", command, true
		}
	}
	if m := plannerHeaderRe.FindStringSubmatch(text); m != nil {
		return "planner", m[1], true
	}
	return "", "", false
}

// messageText extracts the plain text of a user message content field, which
// is either a JSON string or an array of content blocks.
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// parseSession parses one session (main transcript + subagent transcripts)
// into an aggregate. A partial aggregate plus an error may be returned.
func (c *ClaudeSessionUsageCollector) parseSession(sessionPath, subagentDir string) (*UsageAggregate, error) {
	agg := newUsageAggregate()
	timeline, err := c.parseTranscript(sessionPath, agg, nil)
	if err != nil {
		return agg, err
	}
	if len(timeline) == 0 {
		// No maestro envelope: operator session or unrelated project dir
		// that matched the prefix. Nothing to attribute.
		return agg, nil
	}

	subEntries, err := os.ReadDir(subagentDir)
	if err != nil {
		return agg, nil //nolint:nilerr // missing subagent dir is the normal case
	}
	for _, e := range subEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		subPath := filepath.Join(subagentDir, e.Name())
		if _, err := c.parseTranscript(subPath, agg, timeline); err != nil {
			c.logger.Warnf("usage_collect parse subagent %s: %v", subPath, err)
		}
	}
	return agg, nil
}

// parseTranscript scans one JSONL transcript, accumulating assistant usage
// into agg and returning the envelope timeline found in the file.
//
// When parentTimeline is nil the transcript is a main session: usage is
// attributed to the most recent envelope seen so far in the same file (usage
// before the first envelope is dropped). When parentTimeline is non-nil the
// transcript is a subagent file: usage is attributed by timestamp lookup
// against the parent's timeline.
func (c *ClaudeSessionUsageCollector) parseTranscript(
	path string,
	agg *UsageAggregate,
	parentTimeline []attribution,
) ([]attribution, error) {
	f, err := os.Open(path) //nolint:gosec // path is discovered under the claude config dir, not user input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	const maxLine = 32 * 1024 * 1024 // session lines can embed large pastes
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), maxLine)

	var timeline []attribution
	var current *attribution
	seenRequests := make(map[string]struct{})

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec sessionLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // non-JSON or truncated line: skip
		}
		switch rec.Type {
		case "user":
			if rec.Message == nil {
				continue
			}
			agent, command, ok := parseEnvelope(messageText(rec.Message.Content))
			if !ok {
				continue
			}
			at, _ := time.Parse(time.RFC3339, rec.Timestamp)
			attr := attribution{At: at, Agent: agent, Command: command}
			timeline = append(timeline, attr)
			current = &timeline[len(timeline)-1]
		case "assistant":
			if rec.Message == nil || rec.Message.Usage == nil {
				continue
			}
			// Streaming re-emits the same API call across several JSONL
			// records; dedupe on requestId (fall back to message id).
			dedupeKey := rec.RequestID
			if dedupeKey == "" {
				dedupeKey = rec.Message.ID
			}
			if dedupeKey != "" {
				if _, dup := seenRequests[dedupeKey]; dup {
					continue
				}
				seenRequests[dedupeKey] = struct{}{}
			}
			attr := current
			if parentTimeline != nil {
				at, err := time.Parse(time.RFC3339, rec.Timestamp)
				if err != nil {
					continue
				}
				attr = lookupAttribution(parentTimeline, at)
			}
			if attr == nil {
				continue // pre-envelope or non-maestro usage: not counted
			}
			modelID := rec.Message.Model
			if modelID == "<synthetic>" {
				// Locally synthesized records (error placeholders etc.) are
				// not API calls; their usage numbers are not billable.
				continue
			}
			if modelID == "" {
				modelID = "unknown-model"
			}
			totals := rec.Message.Usage.totals()
			addTokens(agg.Agents, attr.Agent, modelID, totals)
			addTokens(agg.Commands, attr.Command, modelID, totals)
		}
	}
	return timeline, scanner.Err()
}

// lookupAttribution returns the latest attribution at or before ts, or nil
// when ts precedes the whole timeline.
func lookupAttribution(timeline []attribution, ts time.Time) *attribution {
	idx := sort.Search(len(timeline), func(i int) bool {
		return timeline[i].At.After(ts)
	}) - 1
	if idx < 0 {
		return nil
	}
	return &timeline[idx]
}
