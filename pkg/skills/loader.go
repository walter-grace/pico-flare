// Package skills loads SKILL.md files and injects them into the agent's context.
// Skills are domain knowledge (how to behave); tools are executable capabilities.
package skills

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Loader loads skills from a workspace directory.
type Loader struct {
	skillsDir string // e.g. /path/to/workspace/skills
}

// NewLoader creates a loader for skills under workspace/skills.
// If workspace is empty, no skills are loaded.
func NewLoader(workspace string) *Loader {
	if workspace == "" {
		return &Loader{}
	}
	abs, _ := filepath.Abs(workspace)
	return &Loader{skillsDir: filepath.Join(abs, "skills")}
}

// LoadAll returns the combined content of all SKILL.md files for context injection.
// Each skill's body (after frontmatter) is included under a "### Skill: name" header.
func (l *Loader) LoadAll() string {
	if l.skillsDir == "" {
		return ""
	}
	entries, err := os.ReadDir(l.skillsDir)
	if err != nil {
		return ""
	}
	var parts []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillPath := filepath.Join(l.skillsDir, e.Name(), "SKILL.md")
		body, ok := l.loadSkill(skillPath)
		if ok && strings.TrimSpace(body) != "" {
			parts = append(parts, "### Skill: "+e.Name()+"\n\n"+body)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n---\n\n")
}

func (l *Loader) loadSkill(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return stripFrontmatter(string(data)), true
}

var frontmatterRe = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n*`)

func stripFrontmatter(content string) string {
	return frontmatterRe.ReplaceAllString(content, "")
}
