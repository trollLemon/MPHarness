package validation_test

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/trollLemon/MPHarness/v2/internal/validation"
)

func TestLoadSkills(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		want    []Skill
		wantErr bool
	}{
		{
			name: "loads H1 names and sorts",
			files: map[string]string{
				"zebra/SKILL.md": "# Zebra handling\ndo zebra things\n",
				"alpha/SKILL.md": "# Alpha guide\n\nline two\n",
				"alpha/extra.md": "ignored",
			},
			want: []Skill{
				{Name: "Alpha guide", Contents: "# Alpha guide\n\nline two"},
				{Name: "Zebra handling", Contents: "# Zebra handling\ndo zebra things"},
			},
		},
		{
			name: "falls back to directory name without H1",
			files: map[string]string{
				"plain/SKILL.md": "no heading here\n",
			},
			want: []Skill{{Name: "plain", Contents: "no heading here"}},
		},
		{
			name: "skips subdirs without SKILL.md and empty files",
			files: map[string]string{
				"empty/SKILL.md":   "   \n",
				"noskill/notes.md": "text",
				"real/SKILL.md":    "# Real\nbody",
			},
			want: []Skill{{Name: "Real", Contents: "# Real\nbody"}},
		},
		{
			name:    "missing dir errors",
			files:   nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "skills")
			if tt.files != nil {
				for name, body := range tt.files {
					path := filepath.Join(dir, name)
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			skills, err := LoadSkills(dir)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadSkills: %v", err)
			}
			if len(skills) != len(tt.want) {
				t.Fatalf("got %d skills, want %d: %+v", len(skills), len(tt.want), skills)
			}
			for i := range tt.want {
				if skills[i].Name != tt.want[i].Name || skills[i].Contents != tt.want[i].Contents {
					t.Errorf("skill %d = %+v, want %+v", i, skills[i], tt.want[i])
				}
			}
		})
	}
}
