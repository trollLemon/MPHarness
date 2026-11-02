package validation

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Skill is one markdown skill loaded from a skills directory.
type Skill struct {
	Name     string
	Contents string
}

// SkillsDirPath normalizes a skills directory and checks that it exists.
func SkillsDirPath(dir string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(dir))
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("skills_dir %q: %w", clean, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("skills_dir %q is not a directory", clean)
	}
	return clean, nil
}

// LoadSkills reads the SKILL.md file in each immediate subdirectory of dir.
// The skill name is the file's first H1 heading, falling back to the
// subdirectory name. Subdirectories without a SKILL.md are ignored.
func LoadSkills(dir string) ([]Skill, error) {
	clean, err := SkillsDirPath(dir)
	if err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(clean)
	if err != nil {
		return nil, fmt.Errorf("open skills_dir %q: %w", clean, err)
	}
	defer func() { _ = root.Close() }()

	entries, err := os.ReadDir(clean)
	if err != nil {
		return nil, fmt.Errorf("read skills_dir %q: %w", clean, err)
	}

	var skills []Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		data, err := root.ReadFile(path.Join(name, "SKILL.md"))
		if err != nil {
			continue
		}
		contents := string(data)
		if strings.TrimSpace(contents) == "" {
			continue
		}
		skills = append(skills, Skill{
			Name:     skillName(contents, name),
			Contents: strings.TrimSpace(contents),
		})
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, nil
}

func skillName(contents, fallback string) string {
	for line := range strings.SplitSeq(contents, "\n") {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "# "); ok {
			if name = strings.TrimSpace(name); name != "" {
				return name
			}
		}
	}
	return fallback
}
