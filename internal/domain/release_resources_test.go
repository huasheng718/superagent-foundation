package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func portableRelease() TeamReleasePayload {
	return TeamReleasePayload{Assets: AgentAssetBundle{Skills: []SkillDefinition{{ID: "portable", Name: "Portable", ProjectID: "team", Files: map[string]string{"SKILL.md": "---\nname: portable\n---\nChecked instructions\n", "references/api.md": "API", "LICENSE": "MIT\n", "NOTICE": "Upstream notice\n"}}}}}
}
func binaryRelease(path string, data []byte, mode uint32) TeamReleasePayload {
	p := portableRelease()
	sum := sha256.Sum256(data)
	p.Assets.Skills[0].FileRefs = []SkillFileRef{{Path: path, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Mode: mode}}
	p.Assets.SkillFiles = map[string]map[string][]byte{"portable": {path: data}}
	return p
}
func TestTeamReleasePortablePackageNegatives(t *testing.T) {
	if err := ValidateTeamReleasePayload(portableRelease(), "team"); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func() TeamReleasePayload{
		"traversal": func() TeamReleasePayload {
			p := portableRelease()
			p.Assets.Skills[0].Files["../escape"] = "x"
			return p
		},
		"backslash": func() TeamReleasePayload {
			p := portableRelease()
			p.Assets.Skills[0].Files[`dir\escape`] = "x"
			return p
		},
		"nul": func() TeamReleasePayload { p := portableRelease(); p.Assets.Skills[0].Files["a\x00b"] = "x"; return p },
		"invalid-path-utf8": func() TeamReleasePayload {
			p := portableRelease()
			p.Assets.Skills[0].Files[string([]byte{255})] = "x"
			return p
		},
		"invalid-text-utf8": func() TeamReleasePayload {
			p := portableRelease()
			p.Assets.Skills[0].Files["text.md"] = string([]byte{255})
			return p
		},
		"case-alias": func() TeamReleasePayload {
			p := portableRelease()
			p.Assets.Skills[0].Files["References/API.md"] = "alias"
			return p
		},
		"directory-alias": func() TeamReleasePayload {
			p := portableRelease()
			p.Assets.Skills[0].Files["references"] = "file"
			return p
		},
		"entry-case": func() TeamReleasePayload {
			p := portableRelease()
			delete(p.Assets.Skills[0].Files, "SKILL.md")
			p.Assets.Skills[0].Files["skill.md"] = "entry"
			return p
		},
		"whitespace-alias": func() TeamReleasePayload {
			p := portableRelease()
			p.Assets.Skills[0].Files["references/api.md "] = "x"
			return p
		},
		"text-binary-alias":     func() TeamReleasePayload { return binaryRelease("SKILL.md", []byte("duplicate"), 0644) },
		"cross-kind-case-alias": func() TeamReleasePayload { return binaryRelease("References/API.md", []byte("duplicate"), 0644) },
		"binary-entry-utf8": func() TeamReleasePayload {
			p := binaryRelease("SKILL.md", []byte{255}, 0644)
			delete(p.Assets.Skills[0].Files, "SKILL.md")
			return p
		},
		"symlink-mode": func() TeamReleasePayload { return binaryRelease("scripts/link", []byte("target"), 0120777) },
		"setuid-mode":  func() TeamReleasePayload { return binaryRelease("scripts/run", []byte("x"), 04755) },
		"oversized-text": func() TeamReleasePayload {
			p := portableRelease()
			p.Assets.Skills[0].Files["large"] = strings.Repeat("x", int(SkillFileMaxBytes)+1)
			return p
		},
		"extra-binary": func() TeamReleasePayload {
			p := portableRelease()
			p.Assets.SkillFiles = map[string]map[string][]byte{"portable": {"unlisted": {1}}}
			return p
		},
	}
	for name, makePayload := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateTeamReleasePayload(makePayload(), "team"); err == nil {
				t.Fatal("unsafe package accepted")
			}
		})
	}
}
func TestTeamReleaseRuntimeIdentityCollisions(t *testing.T) {
	for _, ids := range [][]string{{"skill_a", "skilla"}, {"Skill-A", "skill-a"}, {strings.Repeat("x", 48) + "a", strings.Repeat("x", 48) + "b"}, {"你好", "!!"}} {
		p := portableRelease()
		p.Assets.Skills = nil
		for _, id := range ids {
			p.Assets.Skills = append(p.Assets.Skills, SkillDefinition{ID: id, Name: id, ProjectID: "team", Content: "x"})
		}
		if err := ValidateTeamReleasePayload(p, "team"); err == nil {
			t.Fatalf("runtime path collision accepted: %q", ids)
		}
	}
}
