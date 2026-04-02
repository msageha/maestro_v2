package daemon

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/daemon/persona"
	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/model"
)

// EnvelopeBuilder assembles the dispatch envelope for a task by injecting
// persona, skills, and learnings sections into the task content.
type EnvelopeBuilder struct {
	maestroDir string
	config     model.Config
	clock      Clock
	dl         *DaemonLogger
}

// NewEnvelopeBuilder creates an EnvelopeBuilder.
func NewEnvelopeBuilder(maestroDir string, cfg model.Config, clock Clock, dl *DaemonLogger) *EnvelopeBuilder {
	return &EnvelopeBuilder{
		maestroDir: maestroDir,
		config:     cfg,
		clock:      clock,
		dl:         dl,
	}
}

// BuildTaskContent enriches the task content with persona, skills, and learnings.
// Returns the enriched content or an error (only when skills policy is "error").
func (eb *EnvelopeBuilder) BuildTaskContent(task *model.Task) (string, error) {
	// Sanitize user-supplied content to escape DATA boundary markers BEFORE
	// appending system-generated sections (skills, learnings) whose markers
	// must remain intact.
	content := agent.SanitizeUserContent(task.Content)

	// Inject persona prompt (prepend)
	if task.PersonaHint != "" {
		if section := persona.FormatPersonaSection(task.PersonaHint, eb.maestroDir); section != "" {
			content = section + content
			eb.dl.Logf(LogLevelDebug, "persona_injected task=%s persona=%s", task.ID, task.PersonaHint)
		}
	}

	// Inject skills (append after persona): task-specific skill_refs + shared skills.
	if eb.config.Skills.Enabled {
		skillContent, err := eb.buildSkillsSection(task)
		if err != nil {
			return "", err
		}
		content += skillContent
	}

	// Inject learnings (append after skills)
	if eb.config.Learnings.Enabled {
		lrns, err := learnings.ReadTopKLearnings(eb.maestroDir, eb.config.Learnings, eb.clock.Now())
		if err != nil {
			eb.dl.Logf(LogLevelWarn, "learnings_read_failed task=%s error=%v", task.ID, err)
		} else if section := learnings.FormatLearningsSection(lrns); section != "" {
			content += section
			eb.dl.Logf(LogLevelDebug, "learnings_injected task=%s count=%d", task.ID, len(lrns))
		}
	}

	return content, nil
}

// BuildCommandContent enriches the command content with planner skills.
// Like BuildTaskContent, it loads command-specific skill_refs and auto-injects
// shared skills. Skills referenced in skill_refs are loaded from the "planner"
// role directory with fallback to "share".
func (eb *EnvelopeBuilder) BuildCommandContent(cmd *model.Command) (string, error) {
	content := agent.SanitizeUserContent(cmd.Content)

	if eb.config.Skills.Enabled {
		skillContent, err := eb.buildCommandSkillsSection(cmd)
		if err != nil {
			return "", err
		}
		content += skillContent
	}

	return content, nil
}

// buildCommandSkillsSection loads and formats the skills section for a command.
// It loads command-specific skills from skill_refs AND shared skills automatically.
// Command-specific skills take priority over shared skills with the same name.
func (eb *EnvelopeBuilder) buildCommandSkillsSection(cmd *model.Command) (string, error) {
	skillsDir := filepath.Join(eb.maestroDir, "skills")

	// 1. Load command-specific skills from skill_refs.
	refs := cmd.SkillRefs
	maxRefs := eb.config.Skills.EffectiveMaxRefsPerTask()
	if len(refs) > maxRefs {
		eb.dl.Logf(LogLevelWarn, "skill_refs_truncated command=%s total=%d max=%d", cmd.ID, len(refs), maxRefs)
		refs = refs[:maxRefs]
	}

	policy := eb.config.Skills.EffectiveMissingRefPolicy()
	var loaded []skill.SkillContent
	seen := make(map[string]struct{})
	for _, ref := range refs {
		sc, err := skill.ReadSkillWithRole(skillsDir, ref, "planner")
		if err != nil {
			if policy == "error" {
				return "", errors.Join(err)
			}
			if errors.Is(err, os.ErrNotExist) {
				eb.dl.Logf(LogLevelWarn, "skill_ref_not_found command=%s ref=%s", cmd.ID, ref)
			} else {
				eb.dl.Logf(LogLevelWarn, "skill_read_failed command=%s ref=%s error=%v", cmd.ID, ref, err)
			}
			continue
		}
		loaded = append(loaded, sc)
		seen[sc.ID] = struct{}{}
	}

	// 2. Auto-inject shared skills (passing empty role scans only skills/share/).
	sharedSkills, err := skill.ReadAllSkillsForRole(skillsDir, "", nil)
	if err != nil {
		eb.dl.Logf(LogLevelWarn, "shared_skills_read_failed command=%s error=%v", cmd.ID, err)
	} else {
		for _, sc := range sharedSkills {
			if _, dup := seen[sc.ID]; !dup {
				loaded = append(loaded, sc)
				seen[sc.ID] = struct{}{}
			}
		}
	}

	if section := skill.FormatSkillSection(loaded, eb.config.Skills.EffectiveMaxBodyChars()); section != "" {
		eb.dl.Logf(LogLevelDebug, "planner_skills_injected command=%s count=%d", cmd.ID, len(loaded))
		return section, nil
	}
	return "", nil
}

// buildSkillsSection loads and formats the skills section for a task.
// It loads task-specific skills from skill_refs AND shared skills automatically.
// Task-specific skills take priority over shared skills with the same name.
func (eb *EnvelopeBuilder) buildSkillsSection(task *model.Task) (string, error) {
	skillsDir := filepath.Join(eb.maestroDir, "skills")

	// 1. Load task-specific skills from skill_refs.
	refs := task.SkillRefs
	maxRefs := eb.config.Skills.EffectiveMaxRefsPerTask()
	if len(refs) > maxRefs {
		eb.dl.Logf(LogLevelWarn, "skill_refs_truncated task=%s total=%d max=%d", task.ID, len(refs), maxRefs)
		refs = refs[:maxRefs]
	}

	policy := eb.config.Skills.EffectiveMissingRefPolicy()
	var loaded []skill.SkillContent
	seen := make(map[string]struct{})
	for _, ref := range refs {
		sc, err := skill.ReadSkillWithRole(skillsDir, ref, "worker")
		if err != nil {
			if policy == "error" {
				return "", errors.Join(err)
			}
			// warn policy: log and skip
			if errors.Is(err, os.ErrNotExist) {
				eb.dl.Logf(LogLevelWarn, "skill_ref_not_found task=%s ref=%s", task.ID, ref)
			} else {
				eb.dl.Logf(LogLevelWarn, "skill_read_failed task=%s ref=%s error=%v", task.ID, ref, err)
			}
			continue
		}
		loaded = append(loaded, sc)
		seen[sc.ID] = struct{}{}
	}

	// 2. Auto-inject shared skills (passing empty role scans only skills/share/).
	sharedSkills, err := skill.ReadAllSkillsForRole(skillsDir, "", nil)
	if err != nil {
		eb.dl.Logf(LogLevelWarn, "shared_skills_read_failed task=%s error=%v", task.ID, err)
	} else {
		for _, sc := range sharedSkills {
			if _, dup := seen[sc.ID]; !dup {
				loaded = append(loaded, sc)
				seen[sc.ID] = struct{}{}
			}
		}
	}

	if section := skill.FormatSkillSection(loaded, eb.config.Skills.EffectiveMaxBodyChars()); section != "" {
		eb.dl.Logf(LogLevelDebug, "skills_injected task=%s count=%d", task.ID, len(loaded))
		return section, nil
	}
	return "", nil
}
