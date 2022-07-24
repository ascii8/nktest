// Package nktest provides a nakama test runner.
package nktest

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/cenkalti/backoff/v4"
	dclient "github.com/docker/docker/client"
	_ "github.com/lib/pq"
	"github.com/yookoala/realpath"
	"golang.org/x/mod/semver"
)

// Runner is a nakama test runner.
type Runner struct {
	// name is the name of the module to test.
	name string
	// dir is the working directory.
	dir string
	// cacheDir is the directory for cache.
	cacheDir string
	// volumeDir is the directory for docker volumes.
	volumeDir string
	// postgresVersion is the docker tag of the postgres image.
	postgresVersion string
	// nakamaVersion is the docker tag of the nakama/nakama-pluginbuilder images.
	nakamaVersion string
	// httpClient is the http client to use.
	httpClient *http.Client
	// alwaysPull forces pulling images.
	alwaysPull bool
	// dockerClient is the docker client.
	dockerClient *dclient.Client
	// dockerRegistryURL is the docker registry.
	dockerRegistryURL string
	// dockerTokenURL is the docker token.
	dockerTokenURL string
	// dockerAuthName is the docker auth name.
	dockerAuthName string
	// dockerAuthScope is the docker auth scope mask.
	dockerAuthScope string
	//  dockerPostgresId is the image name for the postgres image.
	dockerPostgresId string
	//  dockerNakamaId is the image name for the nakama image.
	dockerNakamaId string
	// dockerNakamaPluginbuilderId is the image name for the nakama pluginbuilder image.
	dockerNakamaPluginbuilderId string
	// configFilename is the config filename.
	configFilename string
	// configTemplate is the config template.
	configTemplate string
	// dockerNetworkRemoveDelay is the remove delay for the docker network.
	dockerNetworkRemoveDelay time.Duration
	// dockerContainerRemoveDelay is the remove delay for the docker container.
	dockerContainerRemoveDelay time.Duration
	// versionCacheTTL is the version cache ttl.
	versionCacheTTL time.Duration
	// backoffMaxInterval is the max backoff interval.
	backoffMaxInterval time.Duration
	// backoffMaxElapsedTime is the max backoff elapsed time.
	backoffMaxElapsedTime time.Duration
	// stdout is standard out.
	stdout io.Writer
	// stderr is standard error.
	stderr io.Writer
	// buildModules are the module packages to build.
	buildModules []string

	// networkId is the created network id.
	networkId string
	// postgresContainerId is the created postgres container id.
	postgresContainerId string
	// nakamaContainerId is the created nakama container id.
	nakamaContainerId string
	postgresLocal     string
	postgresRemote    string
	grpcLocal         string
	grpcRemote        string
	httpLocal         string
	httpRemote        string
}

// New creates a new nakama test runner.
func New(opts ...Option) *Runner {
	t := &Runner{
		postgresVersion:             "latest",
		httpClient:                  http.DefaultClient,
		dockerRegistryURL:           "https://registry-1.docker.io",
		dockerTokenURL:              "https://auth.docker.io/token",
		dockerAuthName:              "registry.docker.io",
		dockerAuthScope:             "repository:%s:pull",
		dockerPostgresId:            "postgres",
		dockerNakamaId:              "heroiclabs/nakama",
		dockerNakamaPluginbuilderId: "heroiclabs/nakama-pluginbuilder",
		configFilename:              "config.yml",
		configTemplate:              string(configYmlTpl),
		versionCacheTTL:             96 * time.Hour,
		dockerNetworkRemoveDelay:    1500 * time.Millisecond,
		dockerContainerRemoveDelay:  1500 * time.Millisecond,
		backoffMaxInterval:          2 * time.Second,
		backoffMaxElapsedTime:       30 * time.Second,
		stdout:                      os.Stdout,
		stderr:                      os.Stderr,
	}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Write satisfies the io.Writer interface.
func (t *Runner) Write(buf []byte) (int, error) {
	return t.stdout.Write(buf)
}

// Logf logs messages to stdout.
func (t *Runner) Logf(s string, v ...interface{}) {
	t.stdout.Write([]byte(strings.TrimRight(fmt.Sprintf(s, v...), "\r\n") + "\n"))
}

// Errf logs messages to stdout.
func (t *Runner) Errf(s string, v ...interface{}) {
	t.stderr.Write([]byte(strings.TrimRight(fmt.Sprintf(s, v...), "\r\n") + "\n"))
}

// Run handles building the nakama plugin and starting the postgres and
// nakama server docker containers.
func (t *Runner) Run(ctx context.Context) error {
	// setup project working directory
	if err := t.Init(); err != nil {
		return err
	}
	// determine latest nakama version
	if err := t.GetNakamaVersion(ctx); err != nil {
		return err
	}
	t.Logf("NAKAMA VERSION: %s", t.nakamaVersion)
	// retrieve images
	if err := GrabDockerImages(ctx, t,
		t.dockerPostgresId+":"+t.postgresVersion,
		t.dockerNakamaPluginbuilderId+":"+t.nakamaVersion,
		t.dockerNakamaId+":"+t.nakamaVersion,
	); err != nil {
		return fmt.Errorf("unable to retrieve docker images: %w", err)
	}
	// build modules
	if err := t.BuildModules(ctx); err != nil {
		return fmt.Errorf("unable to build modules: %w", err)
	}
	// create network for images
	var err error
	if t.networkId, err = CreateDockerNetwork(ctx, t, t.name, t.dockerNetworkRemoveDelay); err != nil {
		return fmt.Errorf("unable to create network: %w", err)
	}
	// run postgres
	if err := t.RunPostgres(ctx); err != nil {
		return fmt.Errorf("unable to start postgres: %w", err)
	}
	// run nakama
	if err := t.RunNakama(ctx); err != nil {
		return fmt.Errorf("unable to start nakama: %w", err)
	}
	return nil
}

// Init initializes the environment for running nakama-pluginbuilder and nakama
// images.
func (t *Runner) Init() error {
	if t.dockerClient == nil {
		var err error
		if t.dockerClient, err = dclient.NewClientWithOpts(dclient.FromEnv, dclient.WithAPIVersionNegotiation()); err != nil {
			return fmt.Errorf("unable to create docker client: %w", err)
		}
	}
	// use working directory if not set
	if t.dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("unable to get working directory: %w", err)
		}
		t.dir = wd
	}
	var err error
	// setup working directory
	if t.dir, err = realpath.Realpath(t.dir); err != nil {
		return fmt.Errorf("unable to determine real path for %s: %w", t.dir, err)
	}
	// setup name
	if t.name == "" {
		t.name = filepath.Base(t.dir)
	}
	// change to directory
	if err := os.Chdir(t.dir); err != nil {
		return fmt.Errorf("unable to change to working directory %s: %w", t.dir, err)
	}
	// set volume directory
	if t.volumeDir == "" {
		t.volumeDir = filepath.Join(t.dir, ".cache")
		if err := os.MkdirAll(t.volumeDir, 0o755); err != nil {
			return fmt.Errorf("unable to create volume dir %s: %w", t.volumeDir, err)
		}
	}
	// check that volume dir is subdir of working dir
	if err := IsSubDir(t.dir, t.volumeDir); err != nil {
		return fmt.Errorf("%s must be subdir of %s: %w", t.volumeDir, t.dir, err)
	}
	// ensure postgres and nakama volume directories exist
	for _, s := range []string{"nakama", "postgres"} {
		d := filepath.Join(t.volumeDir, s)
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("unable to create cache dir %s: %w", d, err)
		}
	}
	// load config template
	tpl, err := template.New("").Parse(t.configTemplate)
	if err != nil {
		return fmt.Errorf("unable to compile config template: %w", err)
	}
	// exec config template
	buf := new(bytes.Buffer)
	if err := tpl.Execute(buf, map[string]interface{}{
		"name": t.name,
	}); err != nil {
		return fmt.Errorf("unable to execute template: %w", err)
	}
	// write config template
	if err := ioutil.WriteFile(filepath.Join(t.volumeDir, "nakama", t.configFilename), buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("unable to write %s: %w", t.configFilename, err)
	}
	return nil
}

// GetNakamaVersion loads and caches the nakama version.
func (t *Runner) GetNakamaVersion(ctx context.Context) error {
	if t.nakamaVersion != "" {
		return nil
	}
	// get user cache storage
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("unable to get user cache dir: %w", err)
	}
	// create cache dir
	cacheDir = filepath.Join(cacheDir, "nktest")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("unable to create cache dir %s: %w", cacheDir, err)
	}
	cacheFile := filepath.Join(cacheDir, "nakama-version")
	// read cached version
	if t.nakamaVersion, err = ReadCachedFile(cacheFile, t.versionCacheTTL); err == nil {
		return nil
	}
	t.Logf("REFRESHING NAKAMA VERSION")
	// get nakama versions
	nk, err := GetDockerTags(ctx, t, t.dockerNakamaId)
	if err != nil {
		return fmt.Errorf("unable to get tags for %s: %w", t.dockerNakamaId, err)
	}
	t.Logf("AVAILABLE NAKAMA VERSIONS: %s", strings.Join(nk, ", "))
	// get nakama-pluginbuilder versions
	pb, err := GetDockerTags(ctx, t, t.dockerNakamaPluginbuilderId)
	if err != nil {
		return fmt.Errorf("unable to get tags for %s: %w", t.dockerNakamaPluginbuilderId, err)
	}
	t.Logf("AVAILABLE NAKAMA PLUGINBUILDER VERSIONS: %s", strings.Join(pb, ", "))
	re := regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	// create map of nakama-pluginbuilder versions
	m := make(map[string]bool)
	for _, ver := range pb {
		if re.MatchString(ver) {
			m[ver] = true
		}
	}
	// sort nakama versions by semver
	var v []string
	for _, ver := range nk {
		if re.MatchString(ver) {
			v = append(v, "v"+ver)
		}
	}
	semver.Sort(v)
	// determine most recent nakama/nakama-pluginbuilder version
	for i := len(v) - 1; i >= 0; i-- {
		ver := strings.TrimPrefix(v[i], "v")
		if !m[ver] {
			continue
		}
		if err := ioutil.WriteFile(cacheFile, []byte(ver+"\n"), 0o644); err != nil {
			return fmt.Errorf("unable to write nakama version cache file %s: %w", cacheFile, err)
		}
		t.nakamaVersion = ver
		return nil
	}
	return fmt.Errorf("no available version of %s matches available versions for %s", t.dockerNakamaPluginbuilderId, t.dockerNakamaId)
}

// BuildModules builds the nakama modules.
func (t *Runner) BuildModules(ctx context.Context) error {
	for _, dir := range t.buildModules {
		if err := t.BuildModule(ctx, dir); err != nil {
			return fmt.Errorf("unable to build module %q: %w", dir, err)
		}
	}
	return nil
}

// BuildModule builds a nakama plugin module.
func (t *Runner) BuildModule(ctx context.Context, dir string) error {
	var err error
	if dir, err = realpath.Realpath(dir); err != nil {
		return fmt.Errorf("unable to determine real path for %s: %w", dir, err)
	}
	if err := IsSubDir(t.dir, dir); err != nil {
		return fmt.Errorf("%s must be subdir of %s: %w", dir, t.dir, err)
	}
	pkgDir, err := filepath.Rel(t.dir, dir)
	if err != nil {
		return fmt.Errorf("unable to make %s relative to %s: %w", dir, t.dir, err)
	}
	name := filepath.Base(dir)
	id, err := DockerRun(
		ctx,
		t,
		t.dockerNakamaPluginbuilderId+":"+t.nakamaVersion,
		0,
		nil,
		[]string{
			filepath.Join(t.dir) + ":/builder",
			filepath.Join(t.volumeDir, "nakama") + ":/nakama",
		},
		"go",
		"build",
		"-trimpath",
		"-buildmode=plugin",
		"-o=/nakama/modules/"+name+".so",
		"./"+pkgDir,
	)
	if err != nil {
		return fmt.Errorf("unable to run %s: %w", t.dockerNakamaPluginbuilderId, err)
	}
	if err := DockerFollowLogs(ctx, t, id); err != nil {
		return fmt.Errorf("unable to follow logs for %s: %w", id, err)
	}
	if err := DockerWait(ctx, t, id); err != nil {
		return err
	}
	out := filepath.Join(t.volumeDir, "nakama", "modules", name+".so")
	fi, err := os.Stat(out)
	switch {
	case err != nil && errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("missing %s: %w", out, err)
	case err != nil:
		return fmt.Errorf("could not stat %s: %w", out, err)
	}
	t.Logf("BUILT: %s %d", out, fi.Size())
	return nil
}

// Backoff executes f until backoff conditions are met or until f returns nil.
func (t *Runner) Backoff(ctx context.Context, f func() error) error {
	p := backoff.NewExponentialBackOff()
	p.MaxInterval = t.backoffMaxInterval
	p.MaxElapsedTime = t.backoffMaxElapsedTime
	return backoff.Retry(f, backoff.WithContext(p, ctx))
}

// RunPostgres runs the postgres server.
func (t *Runner) RunPostgres(ctx context.Context) error {
	var err error
	if t.postgresContainerId, err = DockerRun(
		ctx,
		t,
		t.dockerPostgresId+":"+t.postgresVersion,
		t.dockerContainerRemoveDelay,
		[]string{
			"listen_addresses = '*'",
			"POSTGRES_PASSWORD=" + t.name,
			"POSTGRES_USER=" + t.name,
			"POSTGRES_DB=" + t.name,
		},
		[]string{
			filepath.Join(t.volumeDir, "postgres") + ":/var/lib/postgresql/data",
		},
	); err != nil {
		return fmt.Errorf("unable to run %s: %w", t.dockerPostgresId, err)
	}
	if err := DockerServiceWait(ctx, t, t.postgresContainerId, "5432/tcp", func(local, remote string) error {
		t.postgresLocal = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", t.name, t.name, local, t.name)
		t.postgresRemote = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", t.name, t.name, remote, t.name)
		db, err := sql.Open("postgres", t.postgresLocal)
		if err != nil {
			return err
		}
		return db.Ping()
	}); err != nil {
		return fmt.Errorf("unable to connect to %s: %w", t.dockerPostgresId, err)
	}
	return nil
}

// RunNakama runs the nakama server.
func (t *Runner) RunNakama(ctx context.Context) error {
	var err error
	if t.nakamaContainerId, err = DockerRun(
		ctx,
		t,
		t.dockerNakamaId+":"+t.nakamaVersion,
		t.dockerContainerRemoveDelay,
		nil,
		[]string{
			filepath.Join(t.volumeDir, "nakama") + ":/nakama/data",
		},
		"/bin/sh",
		"-ecx",
		`/nakama/nakama migrate up `+
			`--database.address=`+t.postgresRemote+` && `+
			`exec /nakama/nakama `+
			`--config=/nakama/data/config.yml `+
			`--database.address=`+t.postgresRemote,
	); err != nil {
		return fmt.Errorf("unable to run %s: %w", t.dockerNakamaId, err)
	}
	if err := DockerFollowLogs(ctx, t, t.nakamaContainerId); err != nil {
		return fmt.Errorf("unable to follow logs for %s: %w", t.nakamaContainerId, err)
	}
	if err := DockerServiceWait(ctx, t, t.nakamaContainerId, "7350/tcp", func(local, remote string) error {
		t.httpLocal = "http://" + local
		t.httpRemote = "http://" + remote
		req, err := http.NewRequestWithContext(ctx, "GET", t.httpLocal, nil)
		if err != nil {
			return err
		}
		cl := &http.Client{}
		res, err := cl.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d != 200", res.StatusCode)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("unable to connect to %s (http): %w", t.dockerNakamaId, err)
	}
	if err := DockerServiceWait(ctx, t, t.nakamaContainerId, "7349/tcp", func(local, remote string) error {
		t.grpcLocal = local
		t.grpcRemote = remote
		return nil
	}); err != nil {
		return fmt.Errorf("unable to connect to %s (grpc): %w", t.dockerNakamaId, err)
	}
	return nil
}

// HttpClient returns the http client for the test runner.
func (t *Runner) HttpClient() *http.Client {
	return t.httpClient
}

// DockerClient returns the docker client for the test runner.
func (t *Runner) DockerClient() *dclient.Client {
	return t.dockerClient
}

// AlwaysPull returns whether or not to always pull an image.
func (t *Runner) AlwaysPull() bool {
	return t.alwaysPull
}

// DockerRegistryURL returns the docker registry URL.
func (t *Runner) DockerRegistryURL() string {
	return t.dockerRegistryURL
}

// DockerTokenURL returns the docker token URL.
func (t *Runner) DockerTokenURL() string {
	return t.dockerTokenURL
}

// DockerAuthName returns the token auth name for docker.
func (t *Runner) DockerAuthName() string {
	return t.dockerAuthName
}

// DockerAuthScope returns the auth scope for a image id.
func (t *Runner) DockerAuthScope(id string) string {
	return fmt.Sprintf(t.dockerAuthScope, id)
}

// NetworkId returns the docker network id.
func (t *Runner) NetworkId() string {
	return t.networkId
}

// Stderr returns stderr.
func (t *Runner) Stderr() io.Writer {
	return t.stderr
}

// Stdout returns stdout.
func (t *Runner) Stdout() io.Writer {
	return t.stdout
}

// PostgresLocal returns the postgres local address.
func (t *Runner) PostgresLocal() string {
	return t.postgresLocal
}

// PostgresRemote returns the postgres remote address.
func (t *Runner) PostgresRemote() string {
	return t.postgresRemote
}

// HttpLocal returns the http local address.
func (t *Runner) HttpLocal() string {
	return t.httpLocal
}

// HttpRemote returns the http remote address.
func (t *Runner) HttpRemote() string {
	return t.httpRemote
}

// GrpcLocal returns the grpc local address.
func (t *Runner) GrpcLocal() string {
	return t.grpcLocal
}

// GrpcRemote returns the grpc remote address.
func (t *Runner) GrpcRemote() string {
	return t.grpcRemote
}

// Name returns the name.
func (t *Runner) Name() string {
	return t.name
}

// Option is a nakama test runner option.
type Option func(*Runner)

// WithName is a nakama test runner option to set the name.
func WithName(name string) Option {
	return func(t *Runner) {
		t.name = name
	}
}

// WithDir is a nakama test runner option to set the root dir.
func WithDir(dir string) Option {
	return func(t *Runner) {
		t.dir = dir
	}
}

// WithCacheDir is a nakama test runner option to set the cacheDir.
func WithCacheDir(cacheDir string) Option {
	return func(t *Runner) {
		t.cacheDir = cacheDir
	}
}

// WithVolumeDir is a nakama test runner option to set the volumeDir.
func WithVolumeDir(volumeDir string) Option {
	return func(t *Runner) {
		t.volumeDir = volumeDir
	}
}

// WithPostgresVersion is a nakama test runner option to set the postgresVersion.
func WithPostgresVersion(postgresVersion string) Option {
	return func(t *Runner) {
		t.postgresVersion = postgresVersion
	}
}

// WithNakamaVersion is a nakama test runner option to set the nakamaVersion.
func WithNakamaVersion(nakamaVersion string) Option {
	return func(t *Runner) {
		t.nakamaVersion = nakamaVersion
	}
}

// WithHttpClient is a nakama test runner option to set the httpClient.
func WithHttpClient(httpClient *http.Client) Option {
	return func(t *Runner) {
		t.httpClient = httpClient
	}
}

// WithAlwaysPull is a nakama test runner option to set the alwaysPull.
func WithAlwaysPull(alwaysPull bool) Option {
	return func(t *Runner) {
		t.alwaysPull = alwaysPull
	}
}

// WithDockerClient is a nakama test runner option to set the dockerClient.
func WithDockerClient(dockerClient *dclient.Client) Option {
	return func(t *Runner) {
		t.dockerClient = dockerClient
	}
}

// WithDockerRegistryURL is a nakama test runner option to set the dockerRegistryURL.
func WithDockerRegistryURL(dockerRegistryURL string) Option {
	return func(t *Runner) {
		t.dockerRegistryURL = dockerRegistryURL
	}
}

// WithDockerTokenURL is a nakama test runner option to set the dockerTokenURL.
func WithDockerTokenURL(dockerTokenURL string) Option {
	return func(t *Runner) {
		t.dockerTokenURL = dockerTokenURL
	}
}

// WithDockerAuthName is a nakama test runner option to set the dockerAuthName.
func WithDockerAuthName(dockerAuthName string) Option {
	return func(t *Runner) {
		t.dockerAuthName = dockerAuthName
	}
}

// WithDockerAuthScope is a nakama test runner option to set the dockerAuthScope.
func WithDockerAuthScope(dockerAuthScope string) Option {
	return func(t *Runner) {
		t.dockerAuthScope = dockerAuthScope
	}
}

// WithConfigFilename is a nakama test runner option to set the configFilename.
func WithConfigFilename(configFilename string) Option {
	return func(t *Runner) {
		t.configFilename = configFilename
	}
}

// WithConfigTemplate is a nakama test runner option to set the configTemplate.
func WithConfigTemplate(configTemplate string) Option {
	return func(t *Runner) {
		t.configTemplate = configTemplate
	}
}

// WithDockerNetworkRemoveDelay is a nakama test runner option to set the dockerNetworkRemoveDelay.
func WithDockerNetworkRemoveDelay(dockerNetworkRemoveDelay time.Duration) Option {
	return func(t *Runner) {
		t.dockerNetworkRemoveDelay = dockerNetworkRemoveDelay
	}
}

// WithDockerContainerRemoveDelay is a nakama test runner option to set the dockerContainerRemoveDelay.
func WithDockerContainerRemoveDelay(dockerContainerRemoveDelay time.Duration) Option {
	return func(t *Runner) {
		t.dockerContainerRemoveDelay = dockerContainerRemoveDelay
	}
}

// WithVersionCacheTTL is a nakama test runner option to set the versionCacheTTL.
func WithVersionCacheTTL(versionCacheTTL time.Duration) Option {
	return func(t *Runner) {
		t.versionCacheTTL = versionCacheTTL
	}
}

// WithBackoffMaxInterval is a nakama test runner option to set the backoffMaxInterval.
func WithBackoffMaxInterval(backoffMaxInterval time.Duration) Option {
	return func(t *Runner) {
		t.backoffMaxInterval = backoffMaxInterval
	}
}

// WithBackoffMaxElapsedTime is a nakama test runner option to set the backoffMaxElapsedTime.
func WithBackoffMaxElapsedTime(backoffMaxElapsedTime time.Duration) Option {
	return func(t *Runner) {
		t.backoffMaxElapsedTime = backoffMaxElapsedTime
	}
}

// WithStdout is a nakama test runner option to set the stdout.
func WithStdout(stdout io.Writer) Option {
	return func(t *Runner) {
		t.stdout = stdout
	}
}

// WithStderr is a nakama test runner option to set the stderr.
func WithStderr(stderr io.Writer) Option {
	return func(t *Runner) {
		t.stderr = stderr
	}
}

// WithBuildModule is a nakama test runner option to add a module path to build.
func WithBuildModule(modulePath string) Option {
	return func(t *Runner) {
		t.buildModules = append(t.buildModules, modulePath)
	}
}

// ReadCachedFile reads a cached file from disk, returns error if the file name
// on disk is past the ttl.
func ReadCachedFile(name string, ttl time.Duration) (string, error) {
	fi, err := os.Stat(name)
	switch {
	case err != nil:
		return "", err
	case fi.IsDir():
		return "", fmt.Errorf("%s is a directory", name)
	case fi.ModTime().Add(ttl).Before(time.Now()):
		return "", fmt.Errorf("%s needs to be refreshed (past %v)", name, ttl)
	}
	buf, err := ioutil.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(buf)), nil
}

// IsSubDir determines if b is subdir of a.
func IsSubDir(a, b string) error {
	if _, err := filepath.Rel(a, b); err != nil {
		return fmt.Errorf("%s is not subdir of %s: %w", b, a, err)
	}
	ai, err := os.Lstat(a)
	if err != nil {
		return fmt.Errorf("%s does not exist", a)
	}
	for b != "" {
		bi, err := os.Lstat(b)
		if err != nil {
			return fmt.Errorf("%s does not exist", b)
		}
		if os.SameFile(ai, bi) {
			return nil
		}
		n := filepath.Dir(b)
		if b == n {
			break
		}
		b = n
	}
	return fmt.Errorf("%s is not a subdir of %s", b, a)
}

//go:embed config.yml.tpl
var configYmlTpl []byte
