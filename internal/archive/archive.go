package archive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"
	"github.com/trollLemon/MPHarness/internal/config"
)

type Payload struct {
	VM            config.VMConfig `json:"vm"`
	Agent         string          `json:"agent"`
	CommandOutput string          `json:"command_output"`
}

func Write(log zerolog.Logger, dst string, payload Payload) error {
	log = log.With().Str("component", "archive").Logger()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir archive dir: %w", err)
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal archive payload: %w", err)
	}

	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("write archive %q: %w", dst, err)
	}

	log.Info().Str("dst", dst).Str("agent", payload.Agent).Str("vm", payload.VM.Name).Msg("archive created")
	return nil
}

func Marshal(payload Payload) ([]byte, error) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal archive payload: %w", err)
	}
	return data, nil
}

func Unmarshal(data []byte) (Payload, error) {
	var p Payload
	if err := json.Unmarshal(data, &p); err != nil {
		return Payload{}, fmt.Errorf("unmarshal archive payload: %w", err)
	}
	return p, nil
}

func ReadFile(log zerolog.Logger, path string) (Payload, error) {
	log = log.With().Str("component", "archive").Logger()

	data, err := os.ReadFile(path)
	if err != nil {
		return Payload{}, fmt.Errorf("read archive %q: %w", path, err)
	}

	payload, err := Unmarshal(data)
	if err != nil {
		return Payload{}, err
	}

	log.Debug().Str("path", path).Str("agent", payload.Agent).Msg("archive read")
	return payload, nil
}

func DefaultArchivePath(baseDir, vmName string) string {
	ts := time.Now().UTC().Format("20060102-150405")
	return filepath.Join(baseDir, fmt.Sprintf("%s-%s.json", vmName, ts))
}
