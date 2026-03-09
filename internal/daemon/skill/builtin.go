package skill

import (
	"fmt"
	"io/fs"
)

// ReadBuiltinSkills reads all SKILL.md files from the embedded filesystem
// under the path "skills/share/<name>/SKILL.md". It returns a slice of
// SkillContent for each valid skill found. Skills with parse errors are skipped.
func ReadBuiltinSkills(fsys fs.FS) ([]SkillContent, error) {
	const baseDir = "skills/share"

	entries, err := fs.ReadDir(fsys, baseDir)
	if err != nil {
		return nil, fmt.Errorf("read builtin skills dir: %w", err)
	}

	var skills []SkillContent
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		path := baseDir + "/" + name + "/SKILL.md"

		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			continue
		}

		meta, body, err := parseFrontmatter(string(data))
		if err != nil {
			continue
		}

		meta.ID = name
		if meta.Name == "" {
			meta.Name = name
		}

		skills = append(skills, SkillContent{
			SkillMetadata: meta,
			Body:          body,
		})
	}

	return skills, nil
}

// ReadBuiltinSkillByName reads a single builtin skill by name from the embedded FS.
func ReadBuiltinSkillByName(fsys fs.FS, skillName string) (SkillContent, error) {
	if !isValidIdentifier(skillName) {
		return SkillContent{}, fmt.Errorf("invalid skill name: %q", skillName)
	}

	path := "skills/share/" + skillName + "/SKILL.md"
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return SkillContent{}, fmt.Errorf("read builtin skill %q: %w", skillName, err)
	}

	meta, body, err := parseFrontmatter(string(data))
	if err != nil {
		return SkillContent{}, fmt.Errorf("parse builtin skill %q: %w", skillName, err)
	}

	meta.ID = skillName
	if meta.Name == "" {
		meta.Name = skillName
	}

	return SkillContent{
		SkillMetadata: meta,
		Body:          body,
	}, nil
}
