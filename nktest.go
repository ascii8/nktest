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

	"github.com/teivah/onecontext"
)

// globalCtx is the global context.
var globalCtx struct {
	stdout     io.Writer
	stderr     io.Writer
	httpClient *http.Client
	ctx        context.Context
	conn       context.Context
	cancel     func()
	r          *Runner
}

func init() {
	// determine if test
	verbose := false
	for _, s := range os.Args {
		if s == "-test.v=true" {
			verbose = true
			break
		}
	}
	SetVerbose(verbose)
}

// SetVerbose sets the global verbose value.
func SetVerbose(verbose bool) {
	stdout, stderr, transport := noop, noop, DefaultTransport
	if verbose {
		stdout = os.Stdout
		stderr = os.Stderr
		transport = NewRoundTripper(
			PrefixedWriter(stdout, strings.Repeat(" ", 16-len(DefaultPrefixOut))+DefaultPrefixOut),
			PrefixedWriter(stderr, strings.Repeat(" ", 16-len(DefaultPrefixIn))+DefaultPrefixIn),
			transport,
		)
	}
	globalCtx.stdout = stdout
	globalCtx.stderr = stderr
	globalCtx.httpClient = &http.Client{
		Transport: transport,
	}
}

// Stdout returns the stdout from the context.
func Stdout(ctx context.Context) io.Writer {
	if stdout, ok := ctx.Value(stdoutKey).(io.Writer); ok {
		return stdout
	}
	return globalCtx.stdout
}

// Stderr returns the stderr from the context.
func Stderr(ctx context.Context) io.Writer {
	if stderr, ok := ctx.Value(stderrKey).(io.Writer); ok {
		return stderr
	}
	return globalCtx.stderr
}

// HttpClient returns the http client from the context.
func HttpClient(ctx context.Context) *http.Client {
	if httpClient, ok := ctx.Value(httpClientKey).(*http.Client); ok && httpClient != nil {
		return httpClient
	}
	return globalCtx.httpClient
}

// New creates a new context.
func New(ctx, conn context.Context, opts ...Option) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		// catch signals, canceling context to cause cleanup
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-ctx.Done():
		case sig := <-ch:
			Logf(ctx, "% 16s: %s", "SIGNAL", sig)
			cancel()
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
	w := logWriter(t.Logf)
	ctx = WithStdout(ctx, w)
	ctx = WithStderr(ctx, w)
	return ctx, cancel, globalCtx.r
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
