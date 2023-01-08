package nktest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"regexp"
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

// NakamaContainerShortName is the nakama short name.
var NakamaContainerShortName = "nk"

// NktestRunnerShortName is the nktest short name.
var NktestRunnerShortName = "xx"

// NakamaBuilderContainerShortName is the nakama builder short name.
var NakamaBuilderContainerShortName = "bd"

// PostgresContainerShortName is the postgres short name.
var PostgresContainerShortName = "pg"

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
	w         io.Writer
	field     string
	shortId   string
	shortName string
}

// NewConsoleWriter creates a zerolog console writer.
func NewConsoleWriter(w io.Writer, field, shortId, shortName string) io.Writer {
	return &consoleWriter{
		w:         w,
		field:     field,
		shortId:   shortId,
		shortName: shortName,
	}
}

// Write satisfies the io.Writer interface.
func (w *consoleWriter) Write(lines []byte) (int, error) {
	now := time.Now()
	for i, buf := range bytes.Split(lines, []byte{'\n'}) {
		if len(bytes.TrimSpace(buf)) == 0 {
			continue
		}
		m := make(map[string]interface{})
		if err := json.Unmarshal(buf, &m); err == nil && len(buf) != 0 && buf[0] == '{' {
			if _, ok := m[w.field]; !ok {
				m[w.field] = w.shortId
			}
			_, cok := m[zerolog.CallerFieldName]
			rv, rok := m["runtime"]
			rs, _ := rv.(string)
			if cok && (!rok || strings.HasPrefix(rs, "go1.")) && w.shortName == NakamaContainerShortName {
				m[zerolog.CallerFieldName] = w.shortName
				// decrease level
				if lv, ok := m[zerolog.LevelFieldName]; ok {
					ls, _ := lv.(string)
					l, err := zerolog.ParseLevel(ls)
					if err != nil {
						l = zerolog.DebugLevel
					}
					m[zerolog.LevelFieldName] = minLevel(l).String()
				}
			}
		} else {
			m[w.field] = w.shortId
			s := string(buf)
			if w.shortName == PostgresContainerShortName {
				s = logRE.ReplaceAllString(s, "")
			}
			m[zerolog.MessageFieldName] = s
		}
		// make sure fields set
		if _, ok := m[zerolog.TimestampFieldName]; !ok {
			m[zerolog.TimestampFieldName] = now.Format(time.RFC3339)
		}
		if _, ok := m[zerolog.CallerFieldName]; !ok {
			m[zerolog.CallerFieldName] = w.shortName
		}
		if _, ok := m[zerolog.LevelFieldName]; !ok {
			m[zerolog.LevelFieldName] = zerolog.TraceLevel.String()
		}
		// determine the level
		lv, _ := m[zerolog.LevelFieldName]
		ls, _ := lv.(string)
		level, err := zerolog.ParseLevel(ls)
		if err != nil {
			level = zerolog.TraceLevel
		}
		if level >= zerolog.GlobalLevel() {
			v, err := json.Marshal(m)
			if err != nil {
				return 0, fmt.Errorf("line %d: %w", i, err)
			}
			_, err = w.w.Write(v)
			if err != nil {
				return 0, fmt.Errorf("line %d: %w", i, err)
			}
		}
	}
	return len(lines), nil
}

// logRE matches postgres log lines.
//
// postgres log levels: DEBUG5, DEBUG4, DEBUG3, DEBUG2, DEBUG1, INFO, NOTICE, WARNING, ERROR, LOG, FATAL, and PANIC
var logRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d+ UTC \[\d+\] (INFO|NOTICE|WARNING|ERROR|LOG|FATAL|PANIC|DEBUG[1-5]):\s+`)

// minLevel returns the minimum zerolog level.
func minLevel(a zerolog.Level) zerolog.Level {
	if a > zerolog.InfoLevel {
		return a
	}
	return zerolog.TraceLevel
}
