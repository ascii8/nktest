package nktest_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
)

func Example() {
	// create context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	urlstr := "http://127.0.0.1:7350/healthcheck"
	req, err := http.NewRequestWithContext(ctx, "GET", urlstr, nil)
	if err != nil {
		log.Fatal(err)
	}
	// create client and execute request
	cl := &http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
		},
	}
	res, err := cl.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Body.Close()
	// check response
	if res.StatusCode != http.StatusOK {
		log.Fatalf("status %d != 200", res.StatusCode)
	}
	// read response
	buf, err := io.ReadAll(res.Body)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("healthcheck:", strconv.Itoa(res.StatusCode), http.StatusText(res.StatusCode), string(bytes.TrimSpace(buf)))
	// Output:
	// healthcheck: 200 OK {}
}
