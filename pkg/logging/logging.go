// Package logging configures the process-wide slog logger: level-gated JSON
// (default) or text on stdout, with output keys a Kubernetes log pipeline
// already understands — "ts" (epoch millis in JSON, matching kubelet's
// component-base encoder; RFC3339Nano in text), lowercase bucketed "level"
// paired with klog's numeric "v" below warn, errors under "err", and a short
// "caller" at debug and below.
package logging

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
)

// LevelTrace is the finest level. Hand-written trace sites use
// slog.Log(ctx, logging.LevelTrace, ...); klog V(n) records bridged by
// BridgeKlog land at slog.Level(-n), so V(5)+ (e.g. the kubeletplugin gRPC
// payload dumps at V(6)) fall into the trace bucket. -8 is therefore also the
// floor on klog depth: V(9) and deeper can never be rendered.
const LevelTrace = slog.Level(-8)

// Env vars follow the project-wide RBLN_DRA_DRIVER_* prefix so generic
// names cannot be captured by unrelated env injection.
const (
	envLogLevel  = "RBLN_DRA_DRIVER_LOG_LEVEL"
	envLogFormat = "RBLN_DRA_DRIVER_LOG_FORMAT"
)

// newLogger builds the logger from already-validated settings.
func newLogger(w io.Writer, lvl slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level: lvl,
		// The caller attr's cost and noise are only worth it at debug and below.
		AddSource:   lvl <= slog.LevelDebug,
		ReplaceAttr: newReplaceAttr(format),
	}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

// New builds a contract logger writing to w. Without it, code outside this
// package cannot produce contract-shaped records: a bare slog.JSONHandler
// writes "ERROR"/"time"/RFC3339 where the contract says "error"/"ts"/epoch
// millis, so tests built on one would pin the wrong vocabulary.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	f, err := parseFormat(format)
	if err != nil {
		return nil, err
	}
	return newLogger(w, lvl, f), nil
}

// SetupFromEnv reads RBLN_DRA_DRIVER_LOG_LEVEL / _LOG_FORMAT, installs the
// process-wide default logger (stdout), and returns the effective settings
// so callers can echo them at startup.
// Empty values default to info/json (the production defaults). Invalid
// values do not kill the process: only the offending variable falls back
// to its default, and a Warn carrying a "fallback" key is emitted through
// the installed logger.
func SetupFromEnv() (level, format string) {
	logger, level, format := setupFromEnv(os.Stdout)
	slog.SetDefault(logger)
	return level, format
}

// setupFromEnv builds the env-configured logger writing to w and emits the
// invalid-value warns through it. Split from SetupFromEnv so tests can
// observe the warn output.
func setupFromEnv(w io.Writer) (logger *slog.Logger, level, format string) {
	lvl, levelErr := parseLevel(os.Getenv(envLogLevel))
	if levelErr != nil {
		lvl = slog.LevelInfo
	}
	format, formatErr := parseFormat(os.Getenv(envLogFormat))
	if formatErr != nil {
		format = "json"
	}
	logger = newLogger(w, lvl, format)
	// With level=error an invalid format's warn is suppressed by the gate —
	// accepted, since the error gate was chosen explicitly.
	if levelErr != nil {
		logger.Warn("Invalid "+envLogLevel+", using default", "err", levelErr, "fallback", "info")
	}
	if formatErr != nil {
		logger.Warn("Invalid "+envLogFormat+", using default", "err", formatErr, "fallback", "json")
	}
	return logger, levelName(lvl), format
}

// BridgeKlog routes klog (kubeletplugin helper, client-go, apimachinery,
// utilruntime) through the installed default logger and raises klog's own
// verbosity to the matching depth. Both halves are required: klog compares
// V(n) against its -v threshold before the slog handler is ever consulted
// (klog.VDepth), so SetSlogLogger alone silently drops every V(n>=1) record.
// Call it after SetupFromEnv, passing the level it returned.
func BridgeKlog(level string) {
	klog.SetSlogLogger(slog.Default())

	lvl, err := parseLevel(level)
	if err != nil {
		lvl = slog.LevelInfo
	}
	// klog registers -v onto the flag set it is handed and keeps the threshold
	// in a package-level var, so a throwaway set reaches it without the
	// binaries having to own a CLI flag.
	var fs flag.FlagSet
	klog.InitFlags(&fs)
	// Set cannot fail: the value is a base-10 int literal.
	_ = fs.Set("v", strconv.Itoa(klogVerbosity(lvl)))
}

// FromContext returns the contract logger carrying whatever values ctx adds.
// The DRA kubeletplugin helper wraps every gRPC call in a context logger
// holding "requestID" and "method" (kubeletplugin/nonblockinggrpcserver.go), so
// a request-path call site that reaches for slog.Default() discards the only
// link between a driver record and the kubelet call that caused it. Routing
// through klog is what preserves those values; on a bare context this is just
// the default logger.
func FromContext(ctx context.Context) *slog.Logger {
	return slog.New(logr.ToSlogHandler(klog.FromContext(ctx)))
}

// WithValues returns a context whose logger also carries args, so every
// downstream FromContext inherits them. This is how a request path attaches its
// own correlation keys — claim identity, for instance — without threading a
// logger parameter through every function it calls.
func WithValues(ctx context.Context, args ...any) context.Context {
	return klog.NewContext(ctx, klog.LoggerWithValues(klog.FromContext(ctx), args...))
}

// klogVerbosity is the klog -v threshold admitting exactly the V(n) records the
// slog gate would render, and no more: a klog V(n) record arrives at
// slog.Level(-n), so the deepest renderable n is -lvl — 4 at debug, 8 at the
// trace floor, 0 at info and above.
func klogVerbosity(lvl slog.Level) int {
	return max(0, -int(lvl))
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "error":
		return slog.LevelError, nil
	case "warning", "warn":
		return slog.LevelWarn, nil
	case "debug":
		return slog.LevelDebug, nil
	case "trace":
		return LevelTrace, nil
	}
	return 0, fmt.Errorf("unknown log level %q (error|warning|warn|info|debug|trace)", s)
}

func parseFormat(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "json":
		return "json", nil
	case "text":
		return "text", nil
	}
	return "", fmt.Errorf("unknown log format %q (json|text)", s)
}

// levelName buckets any slog.Level into the closed contract vocabulary.
// klog records bridged through logr arrive at non-standard levels
// (V(2) → -2 renders as "DEBUG+2", V(6) → -6 as "DEBUG-2"); bucketing keeps
// the output vocabulary to error|warn|info|debug|trace.
func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	case l >= slog.LevelDebug:
		return "debug"
	default:
		return "trace"
	}
}

// newReplaceAttr normalizes slog output to keys a Kubernetes log pipeline
// already understands: "ts", bucketed lowercase "level" plus klog's "v" depth,
// and a zap-style "caller" ("dir/file.go:line") instead of the verbose source
// group. The timestamp encoding depends on format, so this is built per logger.
func newReplaceAttr(format string) func([]string, slog.Attr) slog.Attr {
	return func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) > 0 {
			return a
		}
		switch a.Key {
		case slog.TimeKey:
			// String-valued user "time" attrs pass through; a time-valued one is
			// indistinguishable from the record timestamp and gets rewritten too.
			if a.Value.Kind() != slog.KindTime {
				return a
			}
			a.Key = "ts"
			if format == "text" {
				// Text is the local-debugging format, where a readable
				// timestamp beats aggregator compatibility.
				a.Value = slog.StringValue(a.Value.Time().Format(time.RFC3339Nano))
			} else {
				a.Value = slog.Float64Value(epochMillis(a.Value.Time()))
			}
		case slog.LevelKey:
			lvl, ok := a.Value.Any().(slog.Level)
			if !ok {
				// Already rewritten: this is the inlined "level" string below.
				return a
			}
			if lvl >= slog.LevelWarn {
				a.Value = slog.StringValue(levelName(lvl))
				return a
			}
			// Below warn, carry klog's own V depth next to the bucketed name.
			// Bucketing alone would make V(5) and V(7) indistinguishable, and
			// trace admits up to V(8) — unreadable without a numeric filter.
			// An empty-key group is inlined by the stdlib handlers, which is
			// how one ReplaceAttr call yields two keys.
			return slog.Attr{Key: "", Value: slog.GroupValue(
				slog.String(slog.LevelKey, levelName(lvl)),
				slog.Int("v", klogVerbosity(lvl)),
			)}
		case slog.SourceKey:
			src, ok := a.Value.Any().(*slog.Source)
			if !ok {
				return a
			}
			a.Key = "caller"
			a.Value = slog.StringValue(fmt.Sprintf("%s:%d", trimPath(src.File), src.Line))
		}
		return a
	}
}

// epochMillis matches component-base's JSON encoder (logs/json.
// epochMillisTimeEncoder), so driver records and kubelet records can share one
// "ts" field type in the same index instead of colliding string-vs-number.
func epochMillis(t time.Time) float64 {
	return float64(t.UnixNano()) / float64(time.Millisecond)
}

// trimPath keeps at most the last two path segments for a zap-style short caller.
func trimPath(file string) string {
	idx := strings.LastIndexByte(file, '/')
	if idx == -1 {
		return file
	}
	if idx2 := strings.LastIndexByte(file[:idx], '/'); idx2 != -1 {
		return file[idx2+1:]
	}
	return file
}
