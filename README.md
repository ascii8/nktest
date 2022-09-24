# nktest

Package `nktest` provides a Nakama test runner that makes it easy to build and
test [Nakama module plugins](https://heroiclabs.com) with complex or advanced
game logic using nothing but `go test`.

See also [`github.com/ascii8/nakama-go`](https://github.com/ascii8/nakama-go)
package for a web/realtime Nakama Go client.

[![Tests](https://github.com/ascii8/nktest/workflows/Test/badge.svg)](https://github.com/ascii8/nktest/actions?query=workflow%3ATest)
[![Go Report Card](https://goreportcard.com/badge/github.com/ascii8/nktest)](https://goreportcard.com/report/github.com/ascii8/nktest)
[![Reference](https://godoc.org/github.com/ascii8/nktest?status.svg)](https://pkg.go.dev/github.com/ascii8/nktest)
[![Releases](https://img.shields.io/github/v/release/ascii8/nktest?display_name=tag&sort=semver)](https://github.com/ascii8/nktest/releases)

## overview

This package uses [Podman](https://podman.io) to create "rootless" containers
(specifically, [`docker.io/heroiclabs/nakama-pluginbuilder`](https://hub.docker.com/r/heroiclabs/nakama-pluginbuilder)
and [`docker.io/heroiclabs/nakama`](https://hub.docker.com/r/heroiclabs)) to
build Nakama's Go plugins and launch the necessary server components (PostgreSQL
and Nakama).

Provides a stable, repeatable, and quick mechanism for launching testable
end-to-end Nakama servers for game modules (Go, Lua, or Javascript).

Provides additional transport and logger types for use with Go clients to
readily aid in debugging Nakama's API calls.

## quickstart

Add to package/module:

```go
go get github.com/ascii8/nktest
```

From Go's [`TestMain`](https://pkg.go.dev/testing#hdr-Main), use
[`nktest.Main`](https://pkg.go.dev/github.com/ascii8/nktest#Main) to build Go
modules, and to setup/teardown PostgreSQL and Nakama server containers:

```go
import "github.com/ascii8/nktest"

// TestMain handles setting up and tearing down the postgres and nakama
// containers.
func TestMain(m *testing.M) {
	ctx := context.Background()
	ctx = nktest.WithAlwaysPullFromEnv(ctx, "PULL")
	ctx = nktest.WithHostPortMap(ctx)
	nktest.Main(ctx, m,
		nktest.WithDir("./testdata"),
		nktest.WithBuildConfig("./nksample", nktest.WithDefaultGoEnv(), nktest.WithDefaultGoVolumes()),
	)
}
```

Then, from within a `Test*` func, create a cancelable test context, and a
proxy:

```go
import "github.com/ascii8/nktest"

func TestNakamaHealthcheck(t *testing.T) {
	ctx, cancel, nk := nktest.WithCancel(context.Background(), t)
	defer cancel()
	urlstr, err := nktest.RunProxy(ctx)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	t.Logf("proxy: %s", urlstr)
	req, err := http.NewRequestWithContext(ctx, "GET", urlstr+"/healthcheck", nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	// create a client with compression disabled (makes debugging the API
	// requests/responses easier)
	cl := &http.Client{
		Transport: &http.Transport{
			DisableCompression: true,
		},
	}
	// execute the request
	res, err := cl.Do(req)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	defer res.Body.Close()
	// check response
	if res.StatusCode != http.StatusOK {
		t.Errorf("expected %d, got: %d", http.StatusOK, res.StatusCode)
	}
	t.Logf("healthcheck status: %d", res.StatusCode)
	// display connection information
	t.Logf("grpc: %s", nk.GrpcLocal())
	t.Logf("http: %s", nk.HttpLocal())
	t.Logf("console: %s", nk.ConsoleLocal())
	t.Logf("http_key: %s", nk.HttpKey())
	t.Logf("server_key: %s", nk.ServerKey())
}
```

Use the [`WithHostPortMap()` option](https://pkg.go.dev/github.com/ascii8/nktest#WithHostPortMap),
to publish the Postgres and Nakama server's default ports on the host, and [making
it easy to write `Example*` tests](example_test.go).

For more advanced testing scenarios, [see the `github.com/ascii8/nakama-go`
package](https://github.com/ascii8/nakama-go) for a full featured Go Nakama
client.

## examples

* Testing (`nakama-go` package) - [github.com/ascii8/nakama-go/example_test.go](https://github.com/ascii8/nakama-go/blob/master/example_test.go)
* Testing (this package) - [nktest_test.go](nktest_test.go)
* CI/CD example (GitHub Actions, this repository) - [test.yml](.github/workflows/test.yml)
* Go client for Nakama (usable with `GOOS=js GOOS=wasm`) - [`github.com/ascii8/nakama-go`](https://github.com/ascii8/nakama-go)

See the [Go package documentation](https://pkg.go.dev/github.com/ascii8/nktest)
for package level examples.

## why

While Nakama provides a number of different languages with which to build out
game modules, building large-scale, complex logic for Nakama is best done using
Go. For experienced (and even inexperienced!) Go developers, the `go test`
command is simple, efficient and well understood, and works across platforms.

And, for fully automated deployments of Nakama and game modules, a large amount
of quick and reliable testing is needed to ensure rapid application development,
and continuous integration/deployment (CI/CD).

As such, there was clear motivation to make it easy and repeatable to test
entirely from `go test`,

### why podman

The first version of `nktest` used Docker, but builds and tests were slow as it
was not possible to mount the user's Go build/mod cache directories without
stomping on the local UID/GID and subsequently affecting the read/write
permissions. Thus the change to Podman, which is able to run containers without
root permissions, and can keep user UID/GID's on files.

## notes

macOS:

```sh
# update homebrew formulas and upgrade packages
brew update && brew upgrade

# install podman
brew install podman

# install gpgme (needed for podman's Go binding dependenices)
brew install gpgme

# if unable to do `go test -v` out of the box, re-init the podman machine
podman machine stop podman-machine-default
podman machine rm podman-machine-default
podman machine init -v $HOME:$HOME
podman machine start
```
