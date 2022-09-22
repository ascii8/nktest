package nktest

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
)

// DefaultTranpsort is the default http transport.
var DefaultTransport http.RoundTripper = &http.Transport{
	DisableCompression: true,
}

// RoundTripper is a logging transport.
type RoundTripper struct {
	req       io.Writer
	reqBody   bool
	res       io.Writer
	resBody   bool
	transport http.RoundTripper
}

// NewRoundTripper creates a logging transport.
func NewRoundTripper(req, res io.Writer, transport http.RoundTripper) *RoundTripper {
	return &RoundTripper{
		req:       req,
		reqBody:   true,
		res:       res,
		resBody:   true,
		transport: transport,
	}
}

// DisableReqBody disables logging the request body.
func (t *RoundTripper) DisableReqBody() {
	t.reqBody = false
}

// DisableResBody disables logging the response body.
func (t *RoundTripper) DisableResBody() {
	t.resBody = false
}

// RoundTrip satisfies the http.RoundTripper interface.
func (t *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	transport := t.transport
	if transport == nil {
		transport = DefaultTransport
	}
	reqBody, err := httputil.DumpRequestOut(req, t.reqBody)
	if err != nil {
		return nil, err
	}
	res, err := transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resBody, err := httputil.DumpResponse(res, t.resBody)
	if err != nil {
		return nil, err
	}
	_, _ = t.req.Write(reqBody)
	_, _ = t.res.Write(resBody)
	return res, err
}

// prefixedWriter is a prefixed writer.
type prefixedWriter struct {
	w      io.Writer
	prefix []byte
}

// PrefixedWriter creates a new prefixed writer.
func PrefixedWriter(w io.Writer, prefix string) io.Writer {
	return &prefixedWriter{
		w:      w,
		prefix: []byte(prefix),
	}
}

// Write satisfies the io.Writer interface.
func (w *prefixedWriter) Write(buf []byte) (int, error) {
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

// logWriter wraps writing to a log func.
type logWriter func(string, ...interface{})

// Write satisfies the io.Writer interface.
func (w logWriter) Write(buf []byte) (int, error) {
	w(strings.TrimRight(string(buf), "\n") + "\n")
	return len(buf), nil
}

// noopWriter is a no op writer.
type noopWriter struct{}

// Write satisfies the io.Writer interface.
func (noopWriter) Write(buf []byte) (int, error) {
	return len(buf), nil
}

// NoopWriter creates a no op writer.
func NoopWriter() io.Writer {
	return noop
}

// noop is the noop writer.
var noop io.Writer = &noopWriter{}
