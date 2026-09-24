package validation

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var suspiciousPatterns = []string{
	"ignore previous instructions",
	"ignore all previous instructions",
	"ignore the above",
	"disregard previous",
	"system prompt",
	"you are now",
	"jailbreak",
	"do anything now",
	"dan mode",
	"prompt injection",
	"exfiltrate",
	"override instructions",
	"ignore your instructions",
	"ignore earlier instructions",
}

type ContentFinding struct {
	File    string
	Pattern string
	Snippet string
}

func isTextFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false
	}
	if n == 0 {
		return true
	}
	for _, b := range buf[:n] {
		if b == 0 {
			return false
		}
	}
	return true
}

func ScanContentDir(dir string) ([]ContentFinding, error) {
	clean := filepath.Clean(dir)
	info, err := os.Stat(clean)
	if err != nil {
		return nil, fmt.Errorf("content_dir %q: %w", clean, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("content_dir %q is not a directory", clean)
	}

	var findings []ContentFinding

	err = filepath.WalkDir(clean, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !isTextFile(path) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		const maxRead = 1024 * 1024
		limited := io.LimitReader(f, maxRead+1)
		data, err := io.ReadAll(limited)
		if err != nil {
			return nil
		}
		if len(data) > maxRead {
			data = data[:maxRead]
		}
		content := string(data)
		lower := strings.ToLower(content)
		rel, _ := filepath.Rel(clean, path)
		for _, pat := range suspiciousPatterns {
			if strings.Contains(lower, pat) {
				idx := strings.Index(lower, pat)
				start := idx - 40
				if start < 0 {
					start = 0
				}
				end := idx + len(pat) + 40
				if end > len(content) {
					end = len(content)
				}
				snippet := strings.ReplaceAll(content[start:end], "\n", " ")
				snippet = strings.TrimSpace(snippet)
				if len(snippet) > 120 {
					snippet = snippet[:120] + "…"
				}
				findings = append(findings, ContentFinding{
					File:    rel,
					Pattern: pat,
					Snippet: snippet,
				})
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return findings, nil
}
