package nktest

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
)

// DefaultOutPrefix is the default out prefix.
var DefaultOutPrefix = "-> "

// DefaultInPrefix is the default in prefix.
var DefaultInPrefix = "<- "

// Logger is the interface for a logger/writer.
type Logger interface {
	io.Writer
	Logf(string, ...interface{})
	Errf(string, ...interface{})
	Stdout(string) io.Writer
	Stderr(string) io.Writer
	Transport(http.RoundTripper) http.RoundTripper
}

// log wraps a log func.
type log struct {
	f func(string, ...interface{})
}

// NewLogger creates a new logger/writer.
func NewLogger(f func(string, ...interface{})) Logger {
	return &log{f: f}
}

// TruncatedLogger creates a truncating logger/writer.
func TruncatedLogger() Logger {
	return &log{}
}

// Write satisfies the io.Writer interface.
func (f *log) Write(buf []byte) (int, error) {
	if f.f != nil {
		f.f(string(buf))
	}
	return len(buf), nil
}

// Logf satisfies the Logger interface.
func (f *log) Logf(s string, v ...interface{}) {
	if f.f != nil {
		f.f(s, v...)
	}
}

// Errf satisfies the logger interface.
func (f *log) Errf(s string, v ...interface{}) {
	if f.f != nil {
		f.f(s, v...)
	}
}

// Stdout satisfies the Logger interface.
func (f *log) Stdout(prefix string) io.Writer {
	return NewPrefixedWriter(f, prefix)
}

// Stderr satisfies the Logger interface.
func (f *log) Stderr(prefix string) io.Writer {
	return NewPrefixedWriter(f, prefix)
}

// Transport satisfies the Logger interface.
func (f *log) Transport(transport http.RoundTripper) http.RoundTripper {
	return NewTransport(f.Stdout(DefaultOutPrefix), f.Stdout(DefaultInPrefix), transport)
}

// PrefixedWriter is a prefixed writer.
type PrefixedWriter struct {
	w      io.Writer
	prefix []byte
}

// DefaultTransport is the default http transport used by the log transport.
var DefaultTransport = &http.Transport{
	DisableCompression: true,
}

// Transport is a logging transport.
type Transport struct {
	req       io.Writer
	reqBody   bool
	res       io.Writer
	resBody   bool
	transport http.RoundTripper
}

// NewTransport creates a logging transport.
func NewTransport(req, res io.Writer, transport http.RoundTripper) *Transport {
	if transport == nil {
		transport = DefaultTransport
	}
	return &Transport{
		req:       req,
		res:       res,
		transport: transport,
	}
}

// DisableReqBody disables logging the request body.
func (t *Transport) DisableReqBody() {
	t.reqBody = true
}

// DisableResBody disables logging the response body.
func (t *Transport) DisableResBody() {
	t.resBody = true
}

// RoundTrip satisfies the http.RoundTripper interface.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport := t.transport
	if transport == nil {
		transport = DefaultTransport
	}
	reqBody, err := httputil.DumpRequestOut(req, !t.reqBody)
	if err != nil {
		return nil, err
	}
	res, err := transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resBody, err := httputil.DumpResponse(res, !t.resBody)
	if err != nil {
		return nil, err
	}
	_, _ = t.req.Write(reqBody)
	_, _ = t.res.Write(resBody)
	return res, err
}

// NewPrefixedWriter creates a new prefixed writer.
func NewPrefixedWriter(w io.Writer, prefix string) io.Writer {
	return &PrefixedWriter{
		w:      w,
		prefix: []byte(prefix),
	}
}

// Write satisfies the io.Writer interface.
func (w *PrefixedWriter) Write(buf []byte) (int, error) {
	return w.w.Write(
		append(
			w.prefix,
			append(
				bytes.ReplaceAll(
					bytes.TrimRight(buf, "\r\n"),
					[]byte{'\n'},
					append([]byte{'\n'}, w.prefix...),
				),
				'\n',
			)...,
		),
	)
}
