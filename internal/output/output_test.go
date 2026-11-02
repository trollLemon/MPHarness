package output_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/trollLemon/MPHarness/v2/internal/config"
	. "github.com/trollLemon/MPHarness/v2/internal/output"
)

func testCfg() config.OutputConfig {
	return config.OutputConfig{
		Enabled:          true,
		Mode:             "always",
		MaxCommandSize:   5,
		MaxTotalSize:     512 * 1024 * 1024,
		SearchMaxMatches: 200,
	}
}

func TestCapture(t *testing.T) {
	tests := []struct {
		name       string
		cfg        config.OutputConfig
		cutoff     int64
		out        string
		wantInline string
		wantHandle bool
		wantBytes  int64
	}{
		{
			name:       "empty output returns NoOutput sentinel",
			cfg:        testCfg(),
			cutoff:     100,
			out:        "",
			wantInline: NoOutput,
		},
		{
			name: "auto mode small output below cutoff returns inline",
			cfg: config.OutputConfig{
				Enabled: true, Mode: "auto",
			},
			cutoff:     100,
			out:        "hello\n",
			wantInline: "hello\n",
		},
		{
			name: "auto mode output over cutoff creates handle",
			cfg: config.OutputConfig{
				Enabled: true, Mode: "auto",
			},
			cutoff:     5,
			out:        "line1\nline2\nline3\n",
			wantHandle: true,
			wantBytes:  18,
		},
		{
			name: "always mode captures even small output",
			cfg: config.OutputConfig{
				Enabled: true, Mode: "always",
			},
			cutoff:     1000,
			out:        "hi\n",
			wantHandle: true,
			wantBytes:  3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStore(tt.cfg, tt.cutoff)
			res, err := s.Capture(tt.out)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !tt.wantHandle {
				if res.Handle != nil {
					t.Fatalf("got handle %+v, want inline %q", res.Handle, tt.wantInline)
				}
				if res.Inline != tt.wantInline {
					t.Fatalf("inline = %q, want %q", res.Inline, tt.wantInline)
				}
				return
			}
			if res.Handle == nil {
				t.Fatalf("expected handle, got inline %q", res.Inline)
			}
			if res.Handle.ID == "" {
				t.Fatal("handle ID is empty")
			}
			if res.Handle.Content != tt.out {
				t.Fatalf("content = %q, want %q", res.Handle.Content, tt.out)
			}
			if res.Handle.TotalBytes != tt.wantBytes {
				t.Fatalf("total_bytes = %d, want %d", res.Handle.TotalBytes, tt.wantBytes)
			}
		})
	}
}

func TestSearch(t *testing.T) {
	tests := []struct {
		name         string
		captured     string
		useInvalidID bool
		args         SearchArgs
		wantMatches  []string
		wantTotal    int
		wantErrSub   string
	}{
		{
			name:         "unknown output_id returns lookup error",
			captured:     "alpha beta\n",
			useInvalidID: true,
			args:         SearchArgs{OutputID: "missing", Pattern: "alpha"},
			wantErrSub:   "unknown output_id",
		},
		{
			name:       "empty pattern is rejected",
			captured:   "alpha beta\n",
			args:       SearchArgs{Pattern: "   "},
			wantErrSub: "pattern is required",
		},
		{
			name:       "invalid regex is rejected when fixed_string is false",
			captured:   "alpha beta\n",
			args:       SearchArgs{Pattern: "(["},
			wantErrSub: "invalid",
		},
		{
			name:        "invalid regex succeeds as literal when fixed_string is true",
			captured:    "normal\nhas ([ bracket\n",
			args:        SearchArgs{Pattern: "([", FixedString: true},
			wantMatches: []string{"has ([ bracket"},
			wantTotal:   1,
		},
		{
			name:        "Go regex classes like \\d+ and \\s+ match and return line text",
			captured:    "kernel: 5.15.0-25-generic\npatch state:  applied 42\ndone\n",
			args:        SearchArgs{Pattern: `patch\s+state:\s+applied\s+\d+`},
			wantMatches: []string{"patch state:  applied 42"},
			wantTotal:   1,
		},
		{
			name:        "ignore_case works for regex",
			captured:    "Patch State: Applied\nother\n",
			args:        SearchArgs{Pattern: `patch state: applied`, IgnoreCase: true},
			wantMatches: []string{"Patch State: Applied"},
			wantTotal:   1,
		},
		{
			name:        "ignore_case works for fixed_string",
			captured:    "First\nPatch State: Applied\n",
			args:        SearchArgs{Pattern: "PATCH STATE", FixedString: true, IgnoreCase: true},
			wantMatches: []string{"Patch State: Applied"},
			wantTotal:   1,
		},
		{
			name:        "no match returns empty slice and zero total",
			captured:    "one two\nthree four\n",
			args:        SearchArgs{Pattern: "five"},
			wantMatches: nil,
			wantTotal:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStore(testCfg(), 0)
			res, err := s.Capture(tt.captured)
			if err != nil || res.Handle == nil {
				t.Fatalf("setup capture: res=%+v err=%v", res, err)
			}
			id := res.Handle.ID

			args := tt.args
			if !tt.useInvalidID {
				args.OutputID = id
			}

			got, err := s.Search(args)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.OutputID != id {
				t.Fatalf("OutputID = %q, want %q", got.OutputID, id)
			}
			if got.TotalMatches != tt.wantTotal {
				t.Fatalf("TotalMatches = %d, want %d", got.TotalMatches, tt.wantTotal)
			}
			if len(tt.wantMatches) == 0 {
				if len(got.Matches) != 0 {
					t.Fatalf("Matches = %+v, want empty", got.Matches)
				}
				return
			}
			if !reflect.DeepEqual(got.Matches, tt.wantMatches) {
				t.Fatalf("Matches = %+v, want %+v", got.Matches, tt.wantMatches)
			}
		})
	}
}

func TestRead(t *testing.T) {
	tests := []struct {
		name         string
		captured     string
		useInvalidID bool
		offset       int
		limit        int
		want         string
		wantErrSub   string
	}{
		{
			name:         "unknown output_id returns lookup error",
			captured:     "hello world\n",
			useInvalidID: true,
			offset:       0,
			limit:        5,
			wantErrSub:   "unknown output_id",
		},
		{
			name:       "negative offset is rejected",
			captured:   "hello world\n",
			offset:     -1,
			limit:      5,
			wantErrSub: "offset must be >=0",
		},
		{
			name:     "reads requested slice from offset to limit",
			captured: "hello world\n",
			offset:   0,
			limit:    5,
			want:     "hello",
		},
		{
			name:     "reads substring starting at non-zero offset",
			captured: "hello world\n",
			offset:   6,
			limit:    11,
			want:     "world",
		},
		{
			name:       "offset beyond captured output errors",
			captured:   "hello world\n",
			offset:     13,
			limit:      50,
			wantErrSub: "beyond the captured output",
		},
		{
			name:       "unknown output_id error lists active handles",
			captured:   "first\n",
			useInvalidID: true,
			offset:       0,
			limit:        5,
			wantErrSub:   "active output_ids",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStore(testCfg(), 0)
			res, err := s.Capture(tt.captured)
			if err != nil || res.Handle == nil {
				t.Fatalf("setup capture: res=%+v err=%v", res, err)
			}

			id := res.Handle.ID
			if tt.useInvalidID {
				id = "does-not-exist"
			}

			got, err := s.Read(id, tt.offset, tt.limit)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
