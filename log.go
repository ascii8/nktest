package nktest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// ContainerIdFieldName is the container field name used for logs.
var ContainerIdFieldName = "container_id"

// ContainerEmptyValue is the container empty value.
var ContainerEmptyValue = "--------"

// TimeFormatValue is the time format.
var TimeFormatValue = "2006-01-02 15:04:05"

// NakamaCallerValue is the nakama caller value.
var NakamaCallerValue = "nk"

// NktestCallerValue is the nktest caller value.
var NktestCallerValue = "nktest"

// ReplaceCallerField controls replacing caller field information. Set to false
// when trying to debug nakama itself.
var ReplaceCallerField = true

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

// consoleWriter writes to a zerolog console.
type consoleWriter struct {
	w      io.Writer
	cw     io.Writer
	field  string
	value  string
	prefix []byte
}

// NewConsoleWriter creates a zerolog console writer.
func NewConsoleWriter(w, cw io.Writer, field, value string) io.Writer {
	return &consoleWriter{
		w:      w,
		cw:     cw,
		field:  field,
		value:  value,
		prefix: []byte(value + " "),
	}
}

// Write satisfies the io.Writer interface.
func (w *consoleWriter) Write(lines []byte) (int, error) {
	now := time.Now()
	// add timestamp to prefix, colorized
	prefix := append(w.prefix, fmt.Sprintf("\x1b[%dm%v\x1b[0m", 90, now.Format(TimeFormatValue))+" "...)
	for i, buf := range bytes.Split(lines, []byte{'\n'}) {
		if len(bytes.TrimSpace(buf)) == 0 {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal(buf, &m); err == nil && len(buf) != 0 && buf[0] == '{' {
			if _, ok := m[w.field]; !ok {
				m[w.field] = w.value
			}
			if _, ok := m[zerolog.TimestampFieldName]; !ok {
				m[zerolog.TimestampFieldName] = now.Format(time.RFC3339)
			}
			if ReplaceCallerField {
				cv, cok := m[zerolog.CallerFieldName]
				rv, rok := m["runtime"]
				cs, _ := cv.(string)
				rs, _ := rv.(string)
				switch {
				case cok && (!rok || strings.HasPrefix(rs, "go1.")) && cs != NktestCallerValue:
					m[zerolog.CallerFieldName] = NakamaCallerValue
				case !cok:
					m[zerolog.CallerFieldName] = NktestCallerValue
				}
			}
			v, err := json.Marshal(m)
			if err != nil {
				return 0, fmt.Errorf("line %d: %w", i, err)
			}
			_, err = w.cw.Write(v)
			if err != nil {
				return 0, fmt.Errorf("line %d: %w", i, err)
			}
			continue
		}
		_, err := w.w.Write(
			append(
				prefix,
				append(
					bytes.ReplaceAll(
						bytes.TrimRight(buf, "\r\n"),
						[]byte{'\n'},
						append([]byte{'\n'}, prefix...),
					),
					'\n',
				)...,
			),
		)
		if err != nil {
			return 0, fmt.Errorf("line %d: %w", i, err)
		}
	}
	return len(lines), nil
}
