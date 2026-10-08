package logging

import (
	"bufio"
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"
)

// glogHeader is glog's text header, "Lmmdd hh:mm:ss.uuuuuu threadid file:line] "
// (glog/internal/logsink.Printf). The severity letter, file:line and message
// are kept; glog's own timestamp and thread id are dropped in favour of the
// record's.
var glogHeader = regexp.MustCompile(`^([IWEF])\d{4} \d{2}:\d{2}:\d{2}\.\d{6} +\d+ (\S+:\d+)\] (.*)$`)

// BridgeGlog routes glog output through the default logger. rblnlib-go
// (rsdgroup/rblnsmi) logs via glog, which has no sink API: its records go to
// files under /tmp or, with -logtostderr, as text to os.Stderr, where the
// container runtime interleaves them with the JSON stream. The bridge points
// glog at stderr and swaps os.Stderr for a pipe whose reader re-emits each
// line as a contract record, so glog's INFO/WARNING/ERROR severities and its
// caller survive. Anything else that reaches for os.Stderr at write time is
// relayed as well, tagged "logger":"stderr"; the runtime's own crash output
// writes to fd 2 directly and is unaffected.
//
// A no-op when glog is not linked into the binary (the webhook). Call it after
// SetupFromEnv, passing the level it returned.
func BridgeGlog(level string) {
	if flag.Lookup("logtostderr") == nil {
		return
	}
	// Set cannot fail: the flag exists and "true" parses as a bool.
	_ = flag.Set("logtostderr", "true")
	r, w, err := os.Pipe()
	if err != nil {
		slog.Warn("Cannot bridge glog output, leaving it as text on stderr", "err", err)
		return
	}
	lvl, err := parseLevel(level)
	if err != nil {
		lvl = slog.LevelInfo
	}
	os.Stderr = w
	// The write end is never closed, so the relay lives as long as the process.
	go relayStderr(r, lvl <= slog.LevelDebug)
}

func relayStderr(r io.Reader, withCaller bool) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			emitStderrLine(line, withCaller)
		}
		if err != nil {
			return
		}
	}
}

// emitStderrLine turns one stderr line into a contract record: at glog's own
// severity with glog's file:line as "caller" when the line carries a glog
// header, at info otherwise. The record is built without a PC so the handler
// adds no caller of its own, which would point at this file on every record.
func emitStderrLine(line string, withCaller bool) {
	lvl := slog.LevelInfo
	msg := line
	attrs := []slog.Attr{slog.String("logger", "stderr")}
	if m := glogHeader.FindStringSubmatch(line); m != nil {
		switch m[1] {
		case "W":
			lvl = slog.LevelWarn
		case "E", "F":
			lvl = slog.LevelError
		}
		msg = m[3]
		attrs = []slog.Attr{slog.String("logger", "glog")}
		if withCaller {
			attrs = append(attrs, slog.String("caller", m[2]))
		}
	}
	ctx := context.Background()
	h := slog.Default().Handler()
	if !h.Enabled(ctx, lvl) {
		return
	}
	rec := slog.NewRecord(time.Now(), lvl, msg, 0)
	rec.AddAttrs(attrs...)
	// A relay has nowhere to report a sink failure.
	_ = h.Handle(ctx, rec)
}
