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
	"github.com/msageha/maestro_v2/templates"
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
		if section, found := persona.FormatPersonaSectionWithFS(templates.FS, eb.config.Personas, task.PersonaHint, eb.maestroDir); !found {
			eb.dl.Logf(LogLevelWarn, "persona_not_found task=%s persona_hint=%s", task.ID, task.PersonaHint)
		} else if section != "" {
			content = section + content
			eb.dl.Logf(LogLevelDebug, "persona_injected task=%s persona=%s", task.ID, task.PersonaHint)
		}
	}

	// Inject skills (append after persona)
	if eb.config.Skills.Enabled && len(task.SkillRefs) > 0 {
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

// buildSkillsSection loads and formats the skills section for a task.
func (eb *EnvelopeBuilder) buildSkillsSection(task *model.Task) (string, error) {
	refs := task.SkillRefs
	maxRefs := eb.config.Skills.EffectiveMaxRefsPerTask()
	if len(refs) > maxRefs {
		eb.dl.Logf(LogLevelWarn, "skill_refs_truncated task=%s total=%d max=%d", task.ID, len(refs), maxRefs)
		refs = refs[:maxRefs]
	}

	skillsDir := filepath.Join(eb.maestroDir, "skills")
	policy := eb.config.Skills.EffectiveMissingRefPolicy()
	var loaded []skill.SkillContent
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
	}

	if section := skill.FormatSkillSection(loaded, eb.config.Skills.EffectiveMaxBodyChars()); section != "" {
		eb.dl.Logf(LogLevelDebug, "skills_injected task=%s count=%d", task.ID, len(loaded))
		return section, nil
	}
	return "", nil
}
