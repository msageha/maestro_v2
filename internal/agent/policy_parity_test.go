package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/validate"
)

// TestPolicyParity_BashVsGo asserts that for every input in the parity
// corpus, the bash hook and the Go scaffold (`validate.CheckWorkerPolicy`)
// agree on Allow vs Deny. F-025 step 4 (migration plan) needs this gate
// green before any rule the Go scaffold has translated can be relied on
// for parity-driven shadow-mode comparisons in step 6.
//
// SCOPE: today only the rules already implemented in
// `internal/validate/policy.go` are covered (C1 backtick, D001 rm -rf
// system target, plus a few obvious Allow cases). As more rules are
// translated the corpus must grow with them — every new Go rule MUST land
// with at least one Allow + one Deny entry here so a regression in the
// translation surfaces immediately.
//
// REASON COMPARISON: deny reasons are compared byte-for-byte against the
// corpus. This keeps shadow-mode output useful: a divergence means either
// the allow/deny decision changed or the operator-facing reason changed.
func TestPolicyParity_BashVsGo(t *testing.T) {
	requireJq(t)

	corpus := loadPolicyCorpus(t)
	if len(corpus) == 0 {
		t.Fatal("policy corpus is empty")
	}

	dir := t.TempDir()
	pc := NewPolicyChecker(filepath.Join(dir, ".maestro"))
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		t.Fatalf("WriteHookScript: %v", err)
	}

	for _, tc := range corpus {
		t.Run(tc.Name, func(t *testing.T) {
			payload, err := buildHookInputJSON(tc.input)
			if err != nil {
				t.Fatalf("buildHookInputJSON: %v", err)
			}

			bashOut := runHookScript(t, scriptPath, payload)
			bashDecision := parseBashHookDecision(t, bashOut)

			goDecision := validate.CheckWorkerPolicy(tc.input)

			if bashDecision.Allow != goDecision.Allow {
				t.Fatalf("parity mismatch: bash_allow=%v go_allow=%v\nbash output:\n%s\ngo decision: %+v",
					bashDecision.Allow, goDecision.Allow, bashOut, goDecision)
			}
			if goDecision.Allow != tc.WantAllow {
				t.Fatalf("corpus expectation mismatch: got allow=%v, want allow=%v", goDecision.Allow, tc.WantAllow)
			}
			if !tc.WantAllow {
				if bashDecision.Reason != tc.WantReason {
					t.Errorf("bash reason: got %q, want %q", bashDecision.Reason, tc.WantReason)
				}
				if goDecision.Reason != tc.WantReason {
					t.Errorf("go reason: got %q, want %q", goDecision.Reason, tc.WantReason)
				}
			}
		})
	}
}

type policyCorpusCase struct {
	Name        string            `json:"name"`
	Input       policyCorpusInput `json:"input"`
	WantAllow   bool              `json:"want_allow"`
	WantReason  string            `json:"want_reason,omitempty"`
	Description string            `json:"description,omitempty"`
}

type policyCorpusInput struct {
	ToolName    string `json:"tool_name"`
	Command     string `json:"command,omitempty"`
	FilePath    string `json:"file_path,omitempty"`
	RunOnMain   bool   `json:"run_on_main,omitempty"`
	ProjectRoot string `json:"project_root,omitempty"`
}

func (in policyCorpusInput) hookInput() validate.HookInput {
	return validate.HookInput{
		ToolName:    in.ToolName,
		Command:     in.Command,
		FilePath:    in.FilePath,
		RunOnMain:   in.RunOnMain,
		ProjectRoot: in.ProjectRoot,
	}
}

func loadPolicyCorpus(t *testing.T) []struct {
	Name       string
	input      validate.HookInput
	WantAllow  bool
	WantReason string
} {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "validate", "policy_corpus.json"))
	if err != nil {
		t.Fatalf("read policy corpus: %v", err)
	}
	var diskCases []policyCorpusCase
	if err := json.Unmarshal(raw, &diskCases); err != nil {
		t.Fatalf("parse policy corpus: %v", err)
	}
	cases := make([]struct {
		Name       string
		input      validate.HookInput
		WantAllow  bool
		WantReason string
	}, 0, len(diskCases))
	for _, tc := range diskCases {
		if tc.Name == "" {
			t.Fatal("policy corpus contains case with empty name")
		}
		if !tc.WantAllow && tc.WantReason == "" {
			t.Fatalf("policy corpus case %q denies without want_reason", tc.Name)
		}
		cases = append(cases, struct {
			Name       string
			input      validate.HookInput
			WantAllow  bool
			WantReason string
		}{
			Name:       tc.Name,
			input:      tc.Input.hookInput(),
			WantAllow:  tc.WantAllow,
			WantReason: tc.WantReason,
		})
	}
	return cases
}

type bashHookDecision struct {
	Allow  bool
	Reason string
}

func parseBashHookDecision(t *testing.T, output string) bashHookDecision {
	t.Helper()

	if strings.TrimSpace(output) == "" {
		return bashHookDecision{Allow: true}
	}
	var parsed struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		t.Fatalf("parse bash hook output: %v\noutput:\n%s", err, output)
	}
	if parsed.HookSpecificOutput.PermissionDecision == "deny" {
		return bashHookDecision{Allow: false, Reason: parsed.HookSpecificOutput.PermissionDecisionReason}
	}
	return bashHookDecision{Allow: true}
}

// buildHookInputJSON re-serialises a validate.HookInput into the wire
// format the bash hook expects on stdin. Mirrors the schema the
// `policy_checker_test.go::makeBashInput` helper produces, plus support
// for Write/Edit's file_path field.
func buildHookInputJSON(in validate.HookInput) (string, error) {
	toolInput := map[string]any{}
	if in.Command != "" {
		toolInput["command"] = in.Command
	}
	if in.FilePath != "" {
		toolInput["file_path"] = in.FilePath
	}
	m := map[string]any{
		"tool_name":  in.ToolName,
		"tool_input": toolInput,
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
