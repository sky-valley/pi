package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// I11: pi preserves discovery order (skills.ts loadSkills: user dir first,
// then project, insertion order — no name sort).
func TestLoadSkillsPreservesDiscoveryOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()

	// User skill sorts after the project skill by name — discovery order must
	// still put it first.
	writeSkill(t, filepath.Join(home, ".pi", "agent", "skills", "z-user-skill"),
		"---\nname: z-user-skill\ndescription: user skill\n---\n")
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "a-project-skill"),
		"---\nname: a-project-skill\ndescription: project skill\n---\n")

	skills := LoadSkills(cwd)
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d: %+v", len(skills), skills)
	}
	if skills[0].Name != "z-user-skill" || skills[1].Name != "a-project-skill" {
		t.Fatalf("discovery order not preserved (user dir first): %s, %s", skills[0].Name, skills[1].Name)
	}
}

// I11: a folded block-scalar description (`description: >-`) loads, folded to
// a single line like the YAML parser pi uses.
func TestSkillBlockScalarDescription(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "folded"),
		"---\nname: folded\ndescription: >-\n  Line one of the description\n  continues on line two.\n---\nbody\n")

	skills := LoadSkills(cwd)
	if len(skills) != 1 {
		t.Fatalf("folded-description skill did not load: %+v", skills)
	}
	want := "Line one of the description continues on line two."
	if skills[0].Description != want {
		t.Fatalf("folded description wrong:\n got: %q\nwant: %q", skills[0].Description, want)
	}
}

// Literal block scalars keep newlines; clip chomping keeps one trailing \n,
// strip (-) removes it.
func TestParseFrontmatterBlockScalars(t *testing.T) {
	fm, _ := parseFrontmatter("---\nlit: |\n  a\n  b\nstrip: |-\n  a\n  b\nfold: >\n  a\n  b\n---\nbody")
	if got := fm["lit"].value; got != "a\nb\n" {
		t.Fatalf("literal clip wrong: %q", got)
	}
	if got := fm["strip"].value; got != "a\nb" {
		t.Fatalf("literal strip wrong: %q", got)
	}
	if got := fm["fold"].value; got != "a b\n" {
		t.Fatalf("folded clip wrong: %q", got)
	}
}

// Multi-line plain scalars fold across continuation lines.
func TestParseFrontmatterMultilinePlain(t *testing.T) {
	fm, _ := parseFrontmatter("---\ndescription: starts here\n  and continues here\n---\n")
	if got := fm["description"].value; got != "starts here and continues here" {
		t.Fatalf("multi-line plain scalar wrong: %q", got)
	}
}

// I11: disable-model-invocation requires the strict YAML boolean true.
// A quoted "true" is a string and must NOT disable (pi: `=== true` after a
// real YAML parse).
func TestSkillDisableModelInvocationStrictBool(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "plain-true"),
		"---\nname: plain-true\ndescription: d\ndisable-model-invocation: true\n---\n")
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "quoted-true"),
		"---\nname: quoted-true\ndescription: d\ndisable-model-invocation: \"true\"\n---\n")
	writeSkill(t, filepath.Join(cwd, ".pi", "skills", "yaml-caps-true"),
		"---\nname: yaml-caps-true\ndescription: d\ndisable-model-invocation: True\n---\n")

	byName := map[string]Skill{}
	for _, s := range LoadSkills(cwd) {
		byName[s.Name] = s
	}
	if len(byName) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(byName))
	}
	if !byName["plain-true"].DisableModelInvocation {
		t.Fatal("plain true must disable model invocation")
	}
	if byName["quoted-true"].DisableModelInvocation {
		t.Fatal(`quoted "true" is a string, must NOT disable model invocation`)
	}
	if !byName["yaml-caps-true"].DisableModelInvocation {
		t.Fatal("YAML core-schema True must disable model invocation")
	}
}

// I13(c): name/description length validation counts UTF-16 code units
// (JS String.length), not bytes or runes.
func TestSkillValidationLengthsUTF16(t *testing.T) {
	// 513 astral characters = 513 runes but 1026 UTF-16 units > 1024.
	desc := strings.Repeat("\U0001F600", 513)
	errs := validateDescription(desc)
	if len(errs) != 1 || !strings.Contains(errs[0], "description exceeds 1024 characters (1026)") {
		t.Fatalf("description length must count UTF-16 units: %v", errs)
	}
	// 512 astral characters = 1024 units — exactly at the limit, valid.
	if errs := validateDescription(strings.Repeat("\U0001F600", 512)); len(errs) != 0 {
		t.Fatalf("1024 UTF-16 units should be valid: %v", errs)
	}
}
