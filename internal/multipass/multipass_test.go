package multipass

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func writeFakeBin(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "multipass.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake multipass: %v", err)
	}
	return p
}

const sampleInfoJSON = `{
  "errors": [],
  "info": {
    "test-vm": {
      "zone": {"name": "zone1", "available": true},
      "state": "Running",
      "image_hash": "abcdef123456",
      "image_release": "22.04 LTS",
      "release": "Ubuntu 22.04.4 LTS",
      "cpu_count": "2",
      "load": [0.10, 0.05, 0.02],
      "disks": {"sda1": {"used": "4335702016", "total": "20585787392"}},
      "memory": {"used": 1386844160, "total": 4294967296},
      "ipv4": ["10.86.1.2"],
      "mounts": {}
    }
  }
}`

func TestInfo(t *testing.T) {
	tests := []struct {
		name      string
		script    string
		vm        string
		wantRaw   string
		wantErrIs error
		wantErr   bool
	}{
		{
			name:    "returns raw payload",
			script:  "cat <<'EOF'\n" + sampleInfoJSON + "\nEOF\n",
			vm:      "test-vm",
			wantRaw: sampleInfoJSON,
		},
		{
			name:    "empty name rejected",
			vm:      "  ",
			wantErr: true,
		},
		{
			name: "instance not found maps to sentinel",
			script: `echo "info failed: instance \"ghost\" does not exist" >&2
exit 2
`,
			vm:        "ghost",
			wantErrIs: ErrInstanceNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := "multipass"
			if tt.script != "" {
				bin = writeFakeBin(t, tt.script)
			}
			c := &Client{bin: bin, log: zerolog.Nop()}

			raw, err := c.Info(context.Background(), tt.vm)
			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("expected %v, got %v", tt.wantErrIs, err)
				}
				return
			}
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.vm)
				}
				return
			}
			if err != nil {
				t.Fatalf("Info failed: %v", err)
			}
			if strings.TrimSpace(raw) != strings.TrimSpace(tt.wantRaw) {
				t.Errorf("unexpected raw output:\n%s\nwant:\n%s", raw, tt.wantRaw)
			}
		})
	}
}
