package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/ardanlabs/kronk/sdk/kronk"
	"github.com/ardanlabs/kronk/sdk/kronk/applog"
	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/ardanlabs/kronk/sdk/tools/libs"
	"github.com/ardanlabs/kronk/sdk/tools/models"
	"github.com/rs/zerolog"
)

// InitializeModelFiles initializes llama.cpp libbraries and model files.
func InitializeModelFiles(ctx context.Context, logger zerolog.Logger, modelSource string) (models.Path, error) {
	alog := zerologAdapter(logger)

	libMgr, err := libs.New(libs.WithDetect(ctx, alog))
	if err != nil {
		return models.Path{}, fmt.Errorf("detect native libraries: %w", err)
	}
	if _, err := libMgr.Download(ctx, alog); err != nil {
		return models.Path{}, fmt.Errorf("install native libraries: %w", err)
	}
	if err := kronk.Init(kronk.WithLibPath(libMgr.LibsPath())); err != nil {
		return models.Path{}, fmt.Errorf("initialize kronk: %w", err)
	}

	modelMgr, err := models.New()
	if err != nil {
		return models.Path{}, fmt.Errorf("initialize model manager: %w", err)
	}
	mp, err := modelMgr.Download(ctx, alog, modelSource)
	if err != nil {
		return models.Path{}, fmt.Errorf("install model %q: %w", modelSource, err)
	}

	return mp, nil
}

func NewKronk(mp models.Path, logger zerolog.Logger, contextWindow int) (*kronk.Kronk, error) {

	logger.Info().Msg("Loading model")

	krn, err := kronk.New(
		model.WithModelFiles(mp.ModelFiles),
		model.WithContextWindow(contextWindow),
		model.WithAutoTune(true),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create inference model: %w", err)
	}

	logger.Debug().Str("system_info", fmt.Sprint(krn.SystemInfo())).Msg("system info")

	cfg := krn.ModelConfig()
	mi := krn.ModelInfo()

	nGpuLayers := "all"
	if n := cfg.PtrNGpuLayers; n != nil {
		nGpuLayers = fmt.Sprint(*n)
	}
	splitMode := "auto"
	if sm := cfg.PtrSplitMode; sm != nil {
		splitMode = fmt.Sprint(*sm)
	}

	logger.Debug().
		Str("context_window", fmt.Sprint(cfg.ContextWindow())).
		Str("cache_type_k", fmt.Sprint(cfg.CacheTypeK)).
		Str("cache_type_v", fmt.Sprint(cfg.CacheTypeV)).
		Str("flash_attention", fmt.Sprint(cfg.FlashAttention())).
		Str("prefill_batch_size", fmt.Sprint(cfg.PrefillBatchSize())).
		Str("template", mi.Template.FileName).
		Str("grammar", fmt.Sprint(cfg.DefaultParams.Grammar != "")).
		Str("n_seq_max", fmt.Sprint(cfg.NSeqMax())).
		Str("vram_total_mib", fmt.Sprint(mi.VRAMTotal/(1024*1024))).
		Str("slot_memory_mib", fmt.Sprint(mi.SlotMemory/(1024*1024))).
		Str("model_size_mb", fmt.Sprint(mi.Size/(1000*1000))).
		Str("incremental_cache", fmt.Sprint(cfg.IncrementalCache())).
		Str("n_gpu_layers", nGpuLayers).
		Str("split_mode", splitMode).
		Msg("model config")

	logger.Info().Msg("Finished loading model")

	return krn, nil
}

// zerologAdapter converts a zerolog.Logger into the applog.Logger expected by
// the kronk SDK (func(ctx context.Context, msg string, args ...any)).
// Normal messages are forwarded at Info level; messages carrying an "ERROR"
// key are forwarded at Error level. Trace ID from the context is added when
// present. Change Info to Debug if you want kronk chatter only with -v.
// TODO: find if there is a better way to do this.
func zerologAdapter(logger zerolog.Logger) applog.Logger {
	return func(ctx context.Context, msg string, args ...any) {
		if len(msg) > 0 && msg[0] == '\r' {
			msg = strings.TrimPrefix(msg, "\r")
		}

		isErr := false
		for i := 0; i < len(args)-1; i += 2 {
			if k, ok := args[i].(string); ok && k == "ERROR" && args[i+1] != nil {
				if err, ok := args[i+1].(error); ok && err == nil {
					continue
				}
				isErr = true
				break
			}
		}

		var evt *zerolog.Event
		if isErr {
			evt = logger.Error()
		} else {
			evt = logger.Info()
		}

		if tid := applog.GetTraceID(ctx); tid != applog.NoTraceID && tid != "" {
			evt = evt.Str("trace_id", tid)
		}

		for i := 0; i < len(args); i += 2 {
			k := fmt.Sprint(args[i])
			var v any
			if i+1 < len(args) {
				v = args[i+1]
			} else {
				v = true
			}
			if err, ok := v.(error); ok {
				evt = evt.Str(k, err.Error())
			} else {
				evt = evt.Interface(k, v)
			}
		}

		evt.Msg(msg)
	}
}
