package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkillFile(t *testing.T, path, name, desc string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\n"
	if name != "" {
		body += "name: " + name + "\n"
	}
	if desc != "" {
		body += "description: " + desc + "\n"
	}
	body += "---\n# body\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// findSkill returns the loaded skill with the given name (or nil).
func findSkill(skills []Skill, name string) *Skill {
	for i := range skills {
		if skills[i].Name == name {
			return &skills[i]
		}
	}
	return nil
}

// TestLoadSkillsRootMarkdownChild verifies a direct .md child of the skills root
// is loaded as a skill (pi loadSkillsFromDir: root .md files are skills), in
// addition to SKILL.md-rooted subdirectory skills.
func TestLoadSkillsRootMarkdownChild(t *testing.T) {
	root := t.TempDir()
	// Direct .md child of the root.
	writeSkillFile(t, filepath.Join(root, "quick-tip.md"), "quick-tip", "A root-level markdown skill")
	// A subdirectory skill via SKILL.md.
	writeSkillFile(t, filepath.Join(root, "deep", "SKILL.md"), "deep-skill", "A nested skill")

	skills, _ := loadSkillsFromDir(root)
	if findSkill(skills, "quick-tip") == nil {
		t.Fatalf("root .md skill not loaded: %+v", skills)
	}
	if findSkill(skills, "deep-skill") == nil {
		t.Fatalf("nested SKILL.md skill not loaded: %+v", skills)
	}
}

// TestLoadSkillsSkillRootStopsRecursion verifies a dir with SKILL.md is a skill
// root and its non-SKILL .md children / subdirs are NOT additionally loaded.
func TestLoadSkillsSkillRootStopsRecursion(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "myskill")
	writeSkillFile(t, filepath.Join(skillDir, "SKILL.md"), "myskill", "The skill")
	// Extra .md and nested SKILL.md inside the skill root must be ignored.
	writeSkillFile(t, filepath.Join(skillDir, "extra.md"), "extra", "Should not load")
	writeSkillFile(t, filepath.Join(skillDir, "sub", "SKILL.md"), "subskill", "Should not load")

	skills, _ := loadSkillsFromDir(skillDir)
	if len(skills) != 1 || skills[0].Name != "myskill" {
		t.Fatalf("skill-root recursion not stopped: %+v", skills)
	}
}

// TestLoadSkillsHonorsIgnoreFiles verifies .gitignore/.ignore/.fdignore exclude
// matching skill files/dirs.
func TestLoadSkillsHonorsIgnoreFiles(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, filepath.Join(root, "keep.md"), "keep", "Kept skill")
	writeSkillFile(t, filepath.Join(root, "drop.md"), "drop", "Ignored skill")
	writeSkillFile(t, filepath.Join(root, "secret", "SKILL.md"), "secret-skill", "Ignored dir skill")

	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("drop.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".fdignore"), []byte("secret/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, _ := loadSkillsFromDir(root)
	if findSkill(skills, "keep") == nil {
		t.Fatalf("kept skill missing: %+v", skills)
	}
	if findSkill(skills, "drop") != nil {
		t.Fatalf(".gitignore'd skill should be excluded: %+v", skills)
	}
	if findSkill(skills, "secret-skill") != nil {
		t.Fatalf(".fdignore'd dir skill should be excluded: %+v", skills)
	}
}

// TestLoadSkillsSkipsNodeModules verifies node_modules is never scanned.
func TestLoadSkillsSkipsNodeModules(t *testing.T) {
	root := t.TempDir()
	writeSkillFile(t, filepath.Join(root, "node_modules", "pkg", "SKILL.md"), "dep-skill", "Should be skipped")
	writeSkillFile(t, filepath.Join(root, "real", "SKILL.md"), "real-skill", "Kept")

	skills, _ := loadSkillsFromDir(root)
	if findSkill(skills, "dep-skill") != nil {
		t.Fatalf("node_modules skill should be skipped: %+v", skills)
	}
	if findSkill(skills, "real-skill") == nil {
		t.Fatalf("real skill missing: %+v", skills)
	}
}

// TestSkillNameValidationDiagnostics verifies invalid names emit warning
// diagnostics but still load (description present), and a long description emits
// a warning. A missing description drops the skill entirely.
func TestSkillNameValidationDiagnostics(t *testing.T) {
	root := t.TempDir()
	// Invalid name (uppercase + leading hyphen + consecutive hyphens) but valid desc.
	writeSkillFile(t, filepath.Join(root, "Bad", "SKILL.md"), "-Bad--Name", "ok desc")
	// Over-long description.
	longDesc := strings.Repeat("x", maxSkillDescriptionLength+5)
	writeSkillFile(t, filepath.Join(root, "long", "SKILL.md"), "long-skill", longDesc)
	// Missing description: dropped, no skill.
	writeSkillFile(t, filepath.Join(root, "nodesc", "SKILL.md"), "nodesc-skill", "")

	skills, diags := loadSkillsFromDir(root)

	// Invalid-name skill still loads.
	if findSkill(skills, "-Bad--Name") == nil {
		t.Fatalf("skill with invalid name should still load (desc present): %+v", skills)
	}
	// Missing-description skill is dropped.
	if findSkill(skills, "nodesc-skill") != nil {
		t.Fatalf("skill without description must not load")
	}

	msgs := strings.Join(diagMessages(diags), "|")
	for _, want := range []string{
		"name contains invalid characters",
		"name must not start or end with a hyphen",
		"name must not contain consecutive hyphens",
	} {
		if !strings.Contains(msgs, want) {
			t.Fatalf("missing name diagnostic %q in: %s", want, msgs)
		}
	}
	if !strings.Contains(msgs, "description exceeds 1024 characters") {
		t.Fatalf("missing long-description diagnostic in: %s", msgs)
	}
	if !strings.Contains(msgs, "description is required") {
		t.Fatalf("missing required-description diagnostic in: %s", msgs)
	}
}

func diagMessages(diags []SkillDiagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Message)
	}
	return out
}

// TestValidateNameAccepts verifies a well-formed name produces no errors.
func TestValidateNameAccepts(t *testing.T) {
	if errs := validateName("good-skill-1"); len(errs) != 0 {
		t.Fatalf("valid name rejected: %v", errs)
	}
	if errs := validateName(strings.Repeat("a", maxSkillNameLength+1)); len(errs) == 0 {
		t.Fatalf("over-long name should error")
	}
}
