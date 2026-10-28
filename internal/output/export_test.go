package output

import "context"

// Exported test helpers for functions defined in output.go.
var DecideInline = decideInline
var BuildGrepCmd = buildGrepCmd
var BuildReadCmd = buildReadCmd
var ParseGrepOutput = parseGrepOutput
var ParseGrepLines = parseGrepLines
var ParseExitMarker = parseExitMarker
var CapMatches = capMatches
var CapturePaths = capturePaths
var ClampReadLimit = clampReadLimit
var WrapCapture = wrapCapture

func (s *Store) RunRedirect(ctx context.Context, command, path, marker string) (int, error) {
	return s.runRedirect(ctx, command, path, marker)
}

// SetHandle injects a Handle for testing.
func (s *Store) SetHandle(id string, h Handle) {
	if s.handles == nil {
		s.handles = make(map[string]Handle)
	}
	s.handles[id] = h
}