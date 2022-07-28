package nktest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// globalCtx is the global context.
var globalCtx context.Context

// nkTest is the nakama test runner.
var nkTest *Runner

// TestMain handles setting up and tearing down the postgres and nakama docker
// images.
func TestMain(m *testing.M) {
	var cancel func()
	globalCtx, cancel = context.WithCancel(context.Background())
	go func() {
		// catch signals, canceling context to cause cleanup
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-globalCtx.Done():
		case sig := <-ch:
			fmt.Fprintf(os.Stdout, "SIGNAL: %s\n", sig)
			cancel()
		}
	}()
	code := 0
	pull := os.Getenv("PULL")
	nkTest = New(
		WithAlwaysPull(pull != "" && pull != "false" && pull != "0"),
		WithBuildConfig("./nksample", WithDefaultGoEnv(), WithDefaultGoVolumes()),
	)
	if err := nkTest.Run(globalCtx); err == nil {
		code = m.Run()
	} else {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		code = 1
	}
	cancel()
	<-time.After(2200 * time.Millisecond)
	os.Exit(code)
}

func TestHealthcheck(t *testing.T) {
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()
	urlstr, err := nkTest.RunProxy(ctx, WithLogf(t.Logf))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	urlstr += "/healthcheck"
	t.Logf("url: %s", urlstr)
	req, err := http.NewRequest("GET", urlstr, nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	cl := &http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
		},
	}
	res, err := cl.Do(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("expected %d, got: %d", http.StatusOK, res.StatusCode)
	}
	t.Logf("healthcheck is %d", res.StatusCode)
}

func TestGoEchoSample(t *testing.T) {
	msg := map[string]interface{}{
		"test": "msg",
	}
	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	if err := enc.Encode(msg); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	urlstr := nkTest.HttpLocal() + "/v2/rpc/go_echo_sample?unwrap=true&http_key=" + nkTest.Name()
	t.Logf("url: %s", urlstr)
	req, err := http.NewRequest("POST", urlstr, strings.NewReader(strings.TrimSpace(buf.String())))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{
		Transport: NewLogger(t.Logf).Transport(nil),
	}
	res, err := cl.Do(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected %d, got: %d", http.StatusOK, res.StatusCode)
	}
	var v struct {
		Test string `json:"test"`
	}
	dec := json.NewDecoder(res.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
	if v.Test != "msg" {
		t.Errorf("expected %q, got: %s", "msg", v.Test)
	}
}

func TestKeep(t *testing.T) {
	keep := os.Getenv("KEEP")
	if keep == "" {
		return
	}
	d, err := time.ParseDuration(keep)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()
	local := nkTest.HttpLocal()
	urlstr, err := NewProxy(WithLogger(NewLogger(t.Logf))).Run(ctx, local)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	t.Logf("grpc: %s", nkTest.GrpcLocal())
	t.Logf("http: %s", nkTest.HttpLocal())
	t.Logf("proxy: %s", urlstr)
	select {
	case <-time.After(d):
	case <-globalCtx.Done():
	}
}
