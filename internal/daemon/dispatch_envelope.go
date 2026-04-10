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

// BuildTaskContent enriches the task content with persona, skills, and learnings.
// Returns the enriched content or an error (only when skills policy is "error").
func (disp *Dispatcher) BuildTaskContent(task *model.Task) (string, error) {
	// Sanitize user-supplied content to escape DATA boundary markers BEFORE
	// appending system-generated sections (skills, learnings) whose markers
	// must remain intact.
	content := agent.SanitizeUserContent(task.Content)

	// Inject persona prompt (prepend)
	if task.PersonaHint != "" {
		if section := persona.FormatPersonaSection(task.PersonaHint, disp.maestroDir); section != "" {
			content = section + content
			disp.dl.Logf(LogLevelDebug, "persona_injected task=%s persona=%s", task.ID, task.PersonaHint)
		}
	}

	// Inject skills (append after persona): task-specific skill_refs + shared skills.
	if disp.config.Skills.Enabled {
		skillContent, err := disp.buildSkillsSection(task.SkillRefs, task.ID, "worker")
		if err != nil {
			return "", err
		}
		content += skillContent
	}

	// Inject learnings (append after skills)
	if disp.config.Learnings.Enabled {
		lrns, err := learnings.ReadTopKLearnings(disp.maestroDir, disp.config.Learnings, disp.clock.Now())
		if err != nil {
			disp.dl.Logf(LogLevelWarn, "learnings_read_failed task=%s error=%v", task.ID, err)
		} else if section := learnings.FormatLearningsSection(lrns); section != "" {
			content += section
			disp.dl.Logf(LogLevelDebug, "learnings_injected task=%s count=%d", task.ID, len(lrns))
		}
	}

	return content, nil
}

// BuildCommandContent enriches the command content with planner skills.
// Like BuildTaskContent, it loads command-specific skill_refs and auto-injects
// shared skills. Skills referenced in skill_refs are loaded from the "planner"
// role directory with fallback to "share".
func (disp *Dispatcher) BuildCommandContent(cmd *model.Command) (string, error) {
	content := agent.SanitizeUserContent(cmd.Content)

	if disp.config.Skills.Enabled {
		skillContent, err := disp.buildSkillsSection(cmd.SkillRefs, cmd.ID, "planner")
		if err != nil {
			return "", err
		}
		content += skillContent
	}

	return content, nil
}

// buildSkillsSection loads and formats the skills section for a task or command.
// It loads role-specific skills from skillRefs AND shared skills automatically.
// Role-specific skills take priority over shared skills with the same name.
func (disp *Dispatcher) buildSkillsSection(skillRefs []string, entityID, role string) (string, error) {
	skillsDir := filepath.Join(disp.maestroDir, "skills")

	// 1. Load skills from skill_refs.
	refs := skillRefs
	maxRefs := disp.config.Skills.EffectiveMaxRefsPerTask()
	if len(refs) > maxRefs {
		disp.dl.Logf(LogLevelWarn, "skill_refs_truncated %s=%s total=%d max=%d", role, entityID, len(refs), maxRefs)
		refs = refs[:maxRefs]
	}

	policy := disp.config.Skills.EffectiveMissingRefPolicy()
	loaded := make([]skill.Content, 0, len(refs))
	seen := make(map[string]struct{})
	for _, ref := range refs {
		sc, err := skill.ReadSkillWithRole(skillsDir, ref, role)
		if err != nil {
			if policy == "error" {
				return "", errors.Join(err)
			}
			if errors.Is(err, os.ErrNotExist) {
				disp.dl.Logf(LogLevelWarn, "skill_ref_not_found %s=%s ref=%s", role, entityID, ref)
			} else {
				disp.dl.Logf(LogLevelWarn, "skill_read_failed %s=%s ref=%s error=%v", role, entityID, ref, err)
			}
			continue
		}
		loaded = append(loaded, sc)
		seen[sc.ID] = struct{}{}
	}

	// 2. Auto-inject shared skills (passing empty role scans only skills/share/).
	sharedSkills, err := skill.ReadAllSkillsForRole(skillsDir, "", nil)
	if err != nil {
		disp.dl.Logf(LogLevelWarn, "shared_skills_read_failed %s=%s error=%v", role, entityID, err)
	} else {
		for _, sc := range sharedSkills {
			if _, dup := seen[sc.ID]; !dup {
				loaded = append(loaded, sc)
				seen[sc.ID] = struct{}{}
			}
		}
	}

	if section := skill.FormatSkillSection(loaded, disp.config.Skills.EffectiveMaxBodyChars()); section != "" {
		disp.dl.Logf(LogLevelDebug, "skills_injected %s=%s count=%d", role, entityID, len(loaded))
		return section, nil
	}
	return "", nil
}
