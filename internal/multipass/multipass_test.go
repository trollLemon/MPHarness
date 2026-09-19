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
	bin := writeFakeBin(t, `cat <<'EOF'
`+sampleInfoJSON+`
EOF
`)
	c := &Client{bin: bin, log: zerolog.Nop()}

	raw, err := c.Info(context.Background(), "test-vm")
	if err != nil {
		t.Fatalf("Info failed: %v", err)
	}
	if strings.TrimSpace(raw) != strings.TrimSpace(sampleInfoJSON) {
		t.Errorf("unexpected raw output:\n%s\nwant:\n%s", raw, sampleInfoJSON)
	}
}

func TestInfoEmptyName(t *testing.T) {
	c := &Client{bin: "multipass", log: zerolog.Nop()}
	if _, err := c.Info(context.Background(), "  "); err == nil {
		t.Fatalf("expected error for empty name")
	}
}

func TestInfoInstanceNotFound(t *testing.T) {
	bin := writeFakeBin(t, `echo "info failed: instance \"ghost\" does not exist" >&2
exit 2
`)
	c := &Client{bin: bin, log: zerolog.Nop()}

	_, err := c.Info(context.Background(), "ghost")
	if !errors.Is(err, ErrInstanceNotFound) {
		t.Fatalf("expected ErrInstanceNotFound, got %v", err)
	}
}
