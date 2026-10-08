package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"google.golang.org/grpc/grpclog"
)

// BridgeGrpclog routes grpc-go's internal logs (the kubeletplugin helper's
// servers, the health service) through the default logger. grpc-go's default
// logger captures os.Stderr at package init and writes plain text to it, so it
// bypasses both the JSON contract and BridgeGlog's stderr relay. Call it after
// SetupFromEnv and before any gRPC activity: grpclog.SetLoggerV2 is not
// synchronized.
func BridgeGrpclog() {
	grpclog.SetLoggerV2(grpcLogger{l: slog.Default().With("logger", "grpc")})
}

// grpcLogger adapts grpclog.LoggerV2 onto slog. grpc INFO is connection
// lifecycle chatter and maps to debug; WARNING/ERROR keep their severity. msg
// is grpc's free-form text, which is why every record carries "logger":"grpc".
type grpcLogger struct {
	l *slog.Logger
}

func (g grpcLogger) Info(args ...any)                 { g.l.Debug(fmt.Sprint(args...)) }
func (g grpcLogger) Infoln(args ...any)               { g.l.Debug(sprintlnTrim(args)) }
func (g grpcLogger) Infof(format string, args ...any) { g.l.Debug(fmt.Sprintf(format, args...)) }

func (g grpcLogger) Warning(args ...any)                 { g.l.Warn(fmt.Sprint(args...)) }
func (g grpcLogger) Warningln(args ...any)               { g.l.Warn(sprintlnTrim(args)) }
func (g grpcLogger) Warningf(format string, args ...any) { g.l.Warn(fmt.Sprintf(format, args...)) }

func (g grpcLogger) Error(args ...any)                 { g.l.Error(fmt.Sprint(args...)) }
func (g grpcLogger) Errorln(args ...any)               { g.l.Error(sprintlnTrim(args)) }
func (g grpcLogger) Errorf(format string, args ...any) { g.l.Error(fmt.Sprintf(format, args...)) }

// The Fatal family must terminate the process: the grpclog contract is
// "gRPC ensures that all Fatal logs will exit with os.Exit(1)".
func (g grpcLogger) Fatal(args ...any)   { g.l.Error(fmt.Sprint(args...)); os.Exit(1) }
func (g grpcLogger) Fatalln(args ...any) { g.l.Error(sprintlnTrim(args)); os.Exit(1) }
func (g grpcLogger) Fatalf(format string, args ...any) {
	g.l.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

// V gates grpc's extra-verbose records behind the debug gate, so grpc-go skips
// building them entirely at the default level.
func (g grpcLogger) V(int) bool {
	return g.l.Enabled(context.Background(), slog.LevelDebug)
}

// sprintlnTrim is fmt.Sprintln (space-separated args) without the trailing
// newline, which would break single-line output.
func sprintlnTrim(args []any) string {
	s := fmt.Sprintln(args...)
	return s[:len(s)-1]
}
