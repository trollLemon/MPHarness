package harness

import (
	"strings"
	"testing"
)

func TestOutputRunDir(t *testing.T) {
	dir := OutputRunDir("run-123")
	if dir != "/tmp/mph-output/run-123" {
		t.Fatalf("got %q", dir)
	}
	cmds := outputRunDirCommands("run-123")
	if len(cmds) != 1 {
		t.Fatalf("want 1 cmd, got %v", cmds)
	}
	for _, c := range cmds {
		if strings.Contains(c, "rm ") {
			t.Fatalf("setup must not delete other runs' captures: %q", c)
		}
	}
}
