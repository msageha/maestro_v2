package dispatch

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/daemon/persona"
	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/envelope"
	"github.com/msageha/maestro_v2/internal/model"
)

// BuildTaskContent enriches the task content with persona, skills, and learnings.
// Returns a SanitizedContent (boundary markers escaped, system sections appended)
// or an error (only when skills policy is "error").
func (disp *Dispatcher) BuildTaskContent(task *model.Task) (envelope.SanitizedContent, error) {
	// Sanitize user-supplied content via the typestate builder to escape DATA
	// boundary markers BEFORE appending system-generated sections whose markers
	// must remain intact.
	safe := envelope.NewRawContent(task.Content).Sanitize()

	// Inject persona prompt (prepend)
	if task.PersonaHint != "" {
		if section := persona.FormatPersonaSection(task.PersonaHint, disp.maestroDir); section != "" {
			safe = safe.Prepend(section)
			disp.dl.Logf(core.LogLevelDebug, "persona_injected task=%s persona=%s", task.ID, task.PersonaHint)
		}
	}

	// Inject skills (append after persona): task-specific skill_refs + shared skills.
	if disp.config.Skills.Enabled {
		skillContent, err := disp.buildSkillsSection(task.SkillRefs, task.ID, "worker")
		if err != nil {
			return envelope.SanitizedContent{}, err
		}
		safe = safe.Append(skillContent)
	}

	// Inject learnings (append after skills)
	if disp.config.Learnings.Enabled {
		lrns, err := learnings.ReadTopKLearnings(disp.maestroDir, disp.config.Learnings, disp.clock.Now())
		if err != nil {
			disp.dl.Logf(core.LogLevelWarn, "learnings_read_failed task=%s error=%v", task.ID, err)
		} else if section := learnings.FormatLearningsSection(lrns); section != "" {
			safe = safe.Append(section)
			disp.dl.Logf(core.LogLevelDebug, "learnings_injected task=%s count=%d", task.ID, len(lrns))
		}
	}

	// Inject the proven repair strategy for a retry task's failure pattern
	// (C-5 loop; append after learnings). The provider returns "" for
	// non-retry tasks and for patterns without a successful repair.
	if hint := disp.getRepairHint(); hint != nil {
		if section := hint(task); section != "" {
			safe = safe.Append(section)
			disp.dl.Logf(core.LogLevelInfo, "repair_strategy_injected task=%s original_task=%s", task.ID, task.OriginalTaskID)
		}
	}

	return safe, nil
}

// BuildCommandContent enriches the command content with planner skills.
// Like BuildTaskContent, it loads command-specific skill_refs and auto-injects
// shared skills. Skills referenced in skill_refs are loaded from the "planner"
// role directory with fallback to "share".
func (disp *Dispatcher) BuildCommandContent(cmd *model.Command) (envelope.SanitizedContent, error) {
	safe := envelope.NewRawContent(cmd.Content).Sanitize()

	if disp.config.Skills.Enabled {
		skillContent, err := disp.buildSkillsSection(cmd.SkillRefs, cmd.ID, "planner")
		if err != nil {
			return envelope.SanitizedContent{}, err
		}
		safe = safe.Append(skillContent)
	}

	return safe, nil
}

// skillSearchDirs returns the precedence-ordered skill source directories:
// configured skills.extra_dirs (resolved against the project root) first,
// then the bundled <maestro_dir>/skills catalog. Missing extra dirs are
// skipped with a WARN inside ResolveSearchDirs.
func (disp *Dispatcher) skillSearchDirs() []string {
	bundledDir := filepath.Join(disp.maestroDir, "skills")
	projectRoot := filepath.Dir(disp.maestroDir)
	return skill.ResolveSearchDirs(disp.config.Skills.ExtraDirs, projectRoot, bundledDir, nil)
}

// buildSkillsSection loads and formats the skills section for a task or command.
// It loads role-specific skills from skillRefs AND shared skills automatically.
// Role-specific skills take priority over shared skills with the same name,
// and skills.extra_dirs take priority over the bundled catalog within a scope.
func (disp *Dispatcher) buildSkillsSection(skillRefs []string, entityID, role string) (string, error) {
	skillsDirs := disp.skillSearchDirs()

	// 1. Load skills from skill_refs.
	refs := skillRefs
	maxRefs := disp.config.Skills.EffectiveMaxRefsPerTask()
	if len(refs) > maxRefs {
		disp.dl.Logf(core.LogLevelWarn, "skill_refs_truncated %s=%s total=%d max=%d", role, entityID, len(refs), maxRefs)
		refs = refs[:maxRefs]
	}

	policy := disp.config.Skills.EffectiveMissingRefPolicy()
	loaded := make([]skill.Content, 0, len(refs))
	seen := make(map[string]struct{})
	for _, ref := range refs {
		sc, err := skill.ReadSkillWithRoleDirs(skillsDirs, ref, role, nil)
		if err != nil {
			if policy == "error" {
				return "", errors.Join(err)
			}
			if errors.Is(err, os.ErrNotExist) {
				disp.dl.Logf(core.LogLevelWarn, "skill_ref_not_found %s=%s ref=%s", role, entityID, ref)
			} else {
				disp.dl.Logf(core.LogLevelWarn, "skill_read_failed %s=%s ref=%s error=%v", role, entityID, ref, err)
			}
			continue
		}
		loaded = append(loaded, sc)
		seen[sc.ID] = struct{}{}
	}

	// 2. Auto-inject shared skills (passing empty role scans only skills/share/).
	sharedSkills, err := skill.ReadAllSkillsForRoleDirs(skillsDirs, "", nil)
	if err != nil {
		disp.dl.Logf(core.LogLevelWarn, "shared_skills_read_failed %s=%s error=%v", role, entityID, err)
	} else {
		for _, sc := range sharedSkills {
			if _, dup := seen[sc.ID]; !dup {
				loaded = append(loaded, sc)
				seen[sc.ID] = struct{}{}
			}
		}
	}

	if section := skill.FormatSkillSection(loaded, disp.config.Skills.EffectiveMaxBodyChars()); section != "" {
		disp.dl.Logf(core.LogLevelDebug, "skills_injected %s=%s count=%d", role, entityID, len(loaded))
		return section, nil
	}
	return "", nil
}
