package domain

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// RuntimeResourceName is the portable name used by runtime materialization.
// Release collision checks and runtime paths must use the same projection.
func RuntimeResourceName(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() > 48 {
		return b.String()[:48]
	}
	return b.String()
}

// ValidateReleaseSkill checks the portable archive before JSON publication,
// including names that would alias on a case-insensitive Worker filesystem.
func ValidateReleaseSkill(s SkillDefinition, binary map[string][]byte) error {
	if !utf8.ValidString(s.ID) || !utf8.ValidString(s.Name) || !utf8.ValidString(s.Content) || !utf8.ValidString(s.Description) || strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("skill identity and instruction text must be valid UTF-8")
	}
	if int64(len(s.Content)) > SkillFileMaxBytes || len(s.Files)+len(s.FileRefs) > SkillMaxFileCount {
		return fmt.Errorf("skill instruction/file count exceeds package limits")
	}
	seen := map[string]bool{}
	total := int64(len(s.Content))
	path := func(p string, size int64) error {
		if !utf8.ValidString(p) || strings.TrimSpace(p) != p || strings.ContainsAny(p, "\\\x00") {
			return fmt.Errorf("nonportable skill resource path %q", p)
		}
		if err := ValidateRelativeFilePath(p); err != nil {
			return err
		}
		key := strings.ToLower(p)
		if seen[key] {
			return fmt.Errorf("duplicate or case-colliding skill path %q", p)
		}
		for existing := range seen {
			if strings.HasPrefix(key, existing+"/") || strings.HasPrefix(existing, key+"/") {
				return fmt.Errorf("skill file/directory collision %q", p)
			}
		}
		seen[key] = true
		if size < 0 || size > SkillFileMaxBytes {
			return fmt.Errorf("skill resource exceeds file size limit")
		}
		total += size
		if total > SkillTotalMaxBytes {
			return fmt.Errorf("skill package exceeds total size limit")
		}
		if key == "skill.md" && p != "SKILL.md" {
			return fmt.Errorf("skill entrypoint must be named SKILL.md")
		}
		return nil
	}
	for p, text := range s.Files {
		if !utf8.ValidString(text) {
			return fmt.Errorf("skill text resource %q is not UTF-8", p)
		}
		if err := path(p, int64(len(text))); err != nil {
			return err
		}
	}
	for _, ref := range s.FileRefs {
		if ref.Mode & ^uint32(0o777) != 0 {
			return fmt.Errorf("skill resource must be a regular file without special mode bits")
		}
		if err := path(ref.Path, ref.Size); err != nil {
			return err
		}
		if ref.Path == "SKILL.md" && !utf8.Valid(binary[ref.Path]) {
			return fmt.Errorf("SKILL.md is not UTF-8")
		}
	}
	return nil
}
