package harness

import (
	"strings"
	"testing"
)

func TestOutputRunDir(t *testing.T) {
	tests := []struct {
		name    string
		runID   string
		wantDir string
	}{
		{"simple id", "run-123", "/tmp/mph-output/run-123"},
		{"uuid id", "01a0ec35-3962-7a15-be5e-a9372f3401b7", "/tmp/mph-output/01a0ec35-3962-7a15-be5e-a9372f3401b7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if dir := OutputRunDir(tt.runID); dir != tt.wantDir {
				t.Fatalf("got %q want %q", dir, tt.wantDir)
			}
			cmds := outputRunDirCommands(tt.runID)
			if len(cmds) != 1 {
				t.Fatalf("want 1 cmd, got %v", cmds)
			}
			for _, c := range cmds {
				if strings.Contains(c, "rm ") {
					t.Fatalf("setup must not delete other runs' captures: %q", c)
				}
				if !strings.Contains(c, tt.runID) {
					t.Fatalf("setup must target this run, got %q", c)
				}
			}
		})
	}
}
