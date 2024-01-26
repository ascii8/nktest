// Package nktest provides a Nakama test runner that makes it easy to build and
// test Nakama module plugins with complex, advanced game logic using nothing
// but "go test".
//
// See also github.com/ascii8/nakama-go package for a web/realtime Nakama Go
// client.
package nktest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/teivah/onecontext"
)

// globalCtx is the global context.
var globalCtx struct {
	w      io.Writer
	cw     io.Writer
	l      zerolog.Logger
	cl     *http.Client
	ctx    context.Context
	conn   context.Context
	cancel context.CancelFunc
	r      *Runner
}

func init() {
	level := zerolog.Disabled
	// determine log level
	if s := os.Getenv("LEVEL"); s != "" {
		if l, err := zerolog.ParseLevel(s); err == nil {
			level = l
		}
	}
	if s := os.Getenv("DEBUG"); s != "" && s != "0" && s != "off" && s != "false" {
		level = zerolog.DebugLevel
	}
	if s := os.Getenv("TRACE"); s != "" && s != "0" && s != "off" && s != "false" {
		level = zerolog.TraceLevel
	}
	// when go test -v
	if level == zerolog.Disabled {
		for _, s := range os.Args {
			if s == "-test.v=true" {
				level = zerolog.InfoLevel
				break
			}
		}
	}
	SetLevel(level)
}

// SetLevel sets the global log level.
func SetLevel(level zerolog.Level) {
	// override field names to match nakama's logger (zap)
	zerolog.TimestampFieldName = "ts"
	zerolog.MessageFieldName = "msg"
	zerolog.SetGlobalLevel(level)
	stdout, transport := noop, DefaultTransport
	if level < zerolog.Disabled {
		stdout = os.Stdout
		transport = NewRoundTripper(
			PrefixedWriter(stdout, strings.Repeat(" ", 18-len(DefaultPrefixOut))+DefaultPrefixOut),
			PrefixedWriter(stdout, strings.Repeat(" ", 18-len(DefaultPrefixIn))+DefaultPrefixIn),
			transport,
		)
	}
	// console writer
	cw := zerolog.NewConsoleWriter(func(cw *zerolog.ConsoleWriter) {
		cw.Out = stdout
		cw.TimeFormat = TimeFormatValue
		cw.PartsOrder = []string{ContainerIdFieldName, zerolog.TimestampFieldName, zerolog.LevelFieldName, zerolog.CallerFieldName, zerolog.MessageFieldName}
		cw.FieldsExclude = cw.PartsOrder
	})
	// globals
	globalCtx.w = stdout
	globalCtx.cw = NewConsoleWriter(cw, ContainerIdFieldName, ContainerEmptyValue, NktestRunnerShortName)
	globalCtx.l = zerolog.New(globalCtx.cw).With().Timestamp().Logger()
	globalCtx.cl = &http.Client{
		Transport: transport,
	}
}

// Stdout returns the stdout from the context.
func Stdout(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(stdoutKey).(io.Writer); ok {
		return w
	}
	return globalCtx.w
}

// ConsoleWriter returns the consoleWriter from the context.
func ConsoleWriter(ctx context.Context) io.Writer {
	if cw, ok := ctx.Value(consoleWriterKey).(io.Writer); ok {
		return cw
	}
	return globalCtx.cw
}

// Logger returns the logger from the context.
func Logger(ctx context.Context) zerolog.Logger {
	if l, ok := ctx.Value(loggerKey).(zerolog.Logger); ok {
		return l
	}
	return globalCtx.l
}

// Info returns a info logger from the context.
func Info(ctx context.Context) *zerolog.Event {
	if l, ok := ctx.Value(loggerKey).(zerolog.Logger); ok {
		return l.Info()
	}
	return globalCtx.l.Info()
}

// Debug returns a debug logger from the context.
func Debug(ctx context.Context) *zerolog.Event {
	if l, ok := ctx.Value(loggerKey).(zerolog.Logger); ok {
		return l.Debug()
	}
	return globalCtx.l.Debug()
}

// Trace returns a trace logger from the context.
func Trace(ctx context.Context) *zerolog.Event {
	if l, ok := ctx.Value(loggerKey).(zerolog.Logger); ok {
		return l.Trace()
	}
	return globalCtx.l.Trace()
}

// Err returns a err logger from the context.
func Err(ctx context.Context, err error) *zerolog.Event {
	if l, ok := ctx.Value(loggerKey).(zerolog.Logger); ok {
		return l.Err(err)
	}
	return globalCtx.l.Err(err)
}

// Transport creates a transport from the context.
func Transport(ctx context.Context, transport http.RoundTripper) http.RoundTripper {
	return NewRoundTripper(
		PrefixedWriter(Stdout(ctx), DefaultPrefixOut),
		PrefixedWriter(Stdout(ctx), DefaultPrefixIn),
		transport,
	)
}

// HttpClient returns the http client from the context.
func HttpClient(ctx context.Context) *http.Client {
	if cl, ok := ctx.Value(httpClientKey).(*http.Client); ok && cl != nil {
		return cl
	}
	return globalCtx.cl
}

// New creates a new context.
func New(ctx, conn context.Context, opts ...Option) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		// catch signals, canceling context to cause cleanup
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-ctx.Done():
			Trace(ctx).Err(ctx.Err()).Msg("context done")
		case sig := <-ch:
			Trace(ctx).Str("sig", sig.String()).Msg("signal")
		}
	}()
	if globalCtx.ctx != nil {
		panic("New has already been called")
	}
	globalCtx.ctx, globalCtx.conn, globalCtx.cancel, globalCtx.r = ctx, conn, cancel, NewRunner(opts...)
}

// Cancel cancels the current context.
func Cancel() error {
	if globalCtx.cancel != nil {
		globalCtx.cancel()
		return PodmanWait(globalCtx.conn, globalCtx.r.PodContainerId())
	}
	return nil
}

// WithCancel creates a new context for use within Test* funcs.
func WithCancel(parent context.Context, t TestLogger) (context.Context, context.CancelFunc, *Runner) {
	ctx, cancel := onecontext.Merge(globalCtx.ctx, parent)
	return WithStdout(ctx, logWriter(t.Logf)), cancel, globalCtx.r
}

// Main is the main entry point that should be called from TestMain.
func Main(parent context.Context, m TestRunner, opts ...Option) {
	if parent == nil {
		parent = context.Background()
	}
	// get the podman context
	ctx, conn, err := PodmanOpen(parent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: unable to create podman client: %v\n", err)
		os.Exit(1)
	}
	New(ctx, conn, opts...)
	code := 0
	if err := Run(); err == nil {
		code = m.Run()
	} else {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		code = 1
	}
	if err := Cancel(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	os.Exit(code)
}

// Run runs the global context runner.
func Run() error {
	if globalCtx.r == nil {
		panic("New has not been called")
	}
	return globalCtx.r.Run(globalCtx.ctx)
}

// RunProxy creates and runs a http proxy until the context is closed.
func RunProxy(ctx context.Context, opts ...ProxyOption) (string, error) {
	if globalCtx.r == nil {
		panic("New has not been called")
	}
	return globalCtx.r.RunProxy(ctx, opts...)
}

// TestRunner is the test runner interface. Compatible with stdlib's testing.M.
type TestRunner interface {
	Run() int
}

// TestLogger is the test log interface. Compatible with stdlib's testing.T.
type TestLogger interface {
	Logf(string, ...interface{})
}
