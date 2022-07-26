package nktest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
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

func TestHealthcheck(t *testing.T) {
	req, err := http.NewRequest("GET", nkTest.HttpLocal()+"/healthcheck", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	cl := &http.Client{}
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
	t.Logf("body: %s", buf.String())
	req, err := http.NewRequest("POST", urlstr, strings.NewReader(strings.TrimSpace(buf.String())))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	cl := &http.Client{}
	res, err := cl.Do(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	defer res.Body.Close()
	resBuf, err := httputil.DumpResponse(res, true)
	if err != nil {
		t.Fatalf("unable to dump response: %v", err)
	}
	t.Logf("response:\n%s", string(resBuf))
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
		WithBuildPath("./nksample"),
	)
	if err := nkTest.Run(globalCtx); err == nil {
		code = m.Run()
	} else {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		code = 1
	}
	cancel()
	<-time.After(3 * time.Second)
	os.Exit(code)
}
