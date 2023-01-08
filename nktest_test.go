package nktest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain handles setting up and tearing down the postgres and nakama
// containers.
func TestMain(m *testing.M) {
	ctx := context.Background()
	ctx = WithAlwaysPullFromEnv(ctx, "PULL")
	ctx = WithHostPortMap(ctx)
	Main(
		ctx,
		m,
		WithDir("./testdata"),
		WithBuildConfig("./nksample", WithDefaultGoEnv(), WithDefaultGoVolumes()),
	)
}

func TestHealthcheck(t *testing.T) {
	ctx, cancel, _ := WithCancel(context.Background(), t)
	defer cancel()
	urlstr, err := RunProxy(ctx)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	t.Logf("proxy: %s", urlstr)
	req, err := http.NewRequestWithContext(ctx, "GET", urlstr+"/healthcheck", nil)
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
	t.Logf("healthcheck status: %d", res.StatusCode)
}

func TestGoEchoSample(t *testing.T) {
	ctx, cancel, nk := WithCancel(context.Background(), t)
	defer cancel()
	msg := map[string]interface{}{
		"test": "msg",
	}
	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	if err := enc.Encode(msg); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	urlstr := nk.HttpLocal() + "/v2/rpc/go_echo_sample?unwrap=true&http_key=" + nk.Name()
	t.Logf("url: %s", urlstr)
	req, err := http.NewRequestWithContext(ctx, "POST", urlstr, strings.NewReader(strings.TrimSpace(buf.String())))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{
		Transport: Transport(ctx, nil),
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
	ctx, cancel, nk := WithCancel(context.Background(), t)
	defer cancel()
	urlstr, err := nk.RunProxy(ctx)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	t.Logf("local: %s", nk.HttpLocal())
	t.Logf("grpc: %s", nk.GrpcLocal())
	t.Logf("http: %s", nk.HttpLocal())
	t.Logf("console: %s", nk.ConsoleLocal())
	t.Logf("http_key: %s", nk.HttpKey())
	t.Logf("server_key: %s", nk.ServerKey())
	t.Logf("proxy: %s", urlstr)
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}
