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
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/cenkalti/backoff/v4"
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
	// volumeDir is the directory for container volumes.
	volumeDir string

	// httpClient is the http client to use.
	httpClient *http.Client
	// alwaysPull forces pulling images.
	alwaysPull bool

	// buildConfigs are the module build configs.
	buildConfigs []BuildConfig

	// dockerRegistryURL is the docker registry url.
	dockerRegistryURL string
	// dockerTokenURL is the docker token url.
	dockerTokenURL string
	// dockerAuthName is the docker token auth name.
	dockerAuthName string
	// dockerAuthScope is the docker token auth scope mask.
	dockerAuthScope string

	// postgresImageId is the postgres image id.
	postgresImageId string
	// nakamaImageId is the nakama image id.
	nakamaImageId string
	// pluginbuilderImageId is the nakama pluginbuilder image id.
	pluginbuilderImageId string
	// postgresVersion is the tag of the postgres image.
	postgresVersion string
	// nakamaVersion is the tag of the nakama/nakama-pluginbuilder images.
	nakamaVersion string

	// configFilename is the config filename.
	configFilename string
	// configTemplate is the config template.
	configTemplate string

	// networkRemoveDelay is the network remove delay.
	networkRemoveDelay time.Duration
	// containerRemoveDelay is the container remove delay.
	containerRemoveDelay time.Duration
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

	// podId is the created network id.
	podId string
	// postgresContainerId is the created postgres container id.
	postgresContainerId string
	// nakamaContainerId is the created nakama container id.
	nakamaContainerId string

	// postgresLocal is the local postgres address.
	postgresLocal string
	// postgresRemote is the remote postgres address.
	postgresRemote string
	// grpcLocal is the local grpc address.
	grpcLocal string
	// grpcRemote is the remote grpc address.
	grpcRemote string
	// httpLocal is the local http address.
	httpLocal string
	// httpRemote is the remote http address.
	httpRemote string
}

// New creates a new nakama test runner.
func New(opts ...Option) *Runner {
	t := &Runner{
		httpClient:            http.DefaultClient,
		dockerRegistryURL:     "https://registry-1.docker.io",
		dockerTokenURL:        "https://auth.docker.io/token",
		dockerAuthName:        "registry.docker.io",
		dockerAuthScope:       "repository:%s:pull",
		postgresImageId:       "postgres",
		nakamaImageId:         "heroiclabs/nakama",
		pluginbuilderImageId:  "heroiclabs/nakama-pluginbuilder",
		postgresVersion:       "latest",
		configFilename:        "config.yml",
		configTemplate:        string(configYmlTpl),
		versionCacheTTL:       96 * time.Hour,
		networkRemoveDelay:    1500 * time.Millisecond,
		containerRemoveDelay:  500 * time.Millisecond,
		backoffMaxInterval:    2 * time.Second,
		backoffMaxElapsedTime: 30 * time.Second,
		stdout:                os.Stdout,
		stderr:                os.Stderr,
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
	t.stderr.Write([]byte(strings.TrimRight("ERROR: "+fmt.Sprintf(s, v...), "\r\n") + "\n"))
}

// Run handles building the nakama plugin and starting the postgres and
// nakama server containers.
func (t *Runner) Run(ctx context.Context) error {
	// setup project working directory
	if err := t.Init(); err != nil {
		return err
	}
	// get the podman context
	ctx, conn, err := PodmanContext(ctx)
	if err != nil {
		return fmt.Errorf("unable to create podman client: %w", err)
	}
	// determine latest nakama version
	if err := t.GetNakamaVersion(ctx); err != nil {
		return err
	}
	t.Logf("NAKAMA VERSION: %s", t.nakamaVersion)
	// retrieve images
	if err := PodmanPullImages(
		ctx,
		conn,
		t,
		QualifiedId(t.postgresImageId+":"+t.postgresVersion),
		QualifiedId(t.pluginbuilderImageId+":"+t.nakamaVersion),
		QualifiedId(t.nakamaImageId+":"+t.nakamaVersion),
	); err != nil {
		return fmt.Errorf("unable to retrieve images: %w", err)
	}
	// build modules
	if err := t.BuildModules(ctx, conn); err != nil {
		return fmt.Errorf("unable to build modules: %w", err)
	}
	// create network for images
	if t.podId, err = PodmanCreatePod(
		ctx,
		conn,
		t,
		QualifiedId(t.postgresImageId+":"+t.postgresVersion),
		QualifiedId(t.nakamaImageId+":"+t.nakamaVersion),
	); err != nil {
		return fmt.Errorf("unable to create pod: %w", err)
	}
	// run postgres
	if err := t.RunPostgres(ctx, conn); err != nil {
		return fmt.Errorf("unable to start postgres: %w", err)
	}
	// run nakama
	if err := t.RunNakama(ctx, conn); err != nil {
		return fmt.Errorf("unable to start nakama: %w", err)
	}
	return nil
}

// Init initializes the environment for running nakama-pluginbuilder and nakama
// images.
func (t *Runner) Init() error {
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
	if ver, err := ReadCachedFile(cacheFile, t.versionCacheTTL); err == nil {
		t.nakamaVersion = string(bytes.TrimSpace(ver))
		return nil
	}
	t.Logf("REFRESHING NAKAMA VERSION")
	// get nakama versions
	nk, err := GetImageTags(ctx, t, t.nakamaImageId)
	if err != nil {
		return fmt.Errorf("unable to get tags for %s: %w", t.nakamaImageId, err)
	}
	t.Logf("AVAILABLE NAKAMA VERSIONS: %s", strings.Join(nk, ", "))
	// get nakama-pluginbuilder versions
	pb, err := GetImageTags(ctx, t, t.pluginbuilderImageId)
	if err != nil {
		return fmt.Errorf("unable to get tags for %s: %w", t.pluginbuilderImageId, err)
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
	return fmt.Errorf("no available version of %s matches available versions for %s", t.pluginbuilderImageId, t.nakamaImageId)
}

// BuildModules builds the nakama modules.
func (t *Runner) BuildModules(ctx, conn context.Context) error {
	for i, bc := range t.buildConfigs {
		if err := t.BuildModule(ctx, conn, &bc); err != nil {
			return fmt.Errorf("unable to build module %d: %w", i, err)
		}
	}
	return nil
}

// BuildModule builds a nakama plugin module.
func (t *Runner) BuildModule(ctx, conn context.Context, bc *BuildConfig) error {
	// check module path
	if bc.modulePath == "" {
		return fmt.Errorf("must supply module path")
	}
	dir, err := realpath.Realpath(bc.modulePath)
	if err != nil {
		return fmt.Errorf("unable to determine real path for %s: %w", dir, err)
	}
	// ensure module path is sub dir
	if err := IsSubDir(t.dir, dir); err != nil {
		return fmt.Errorf("%s must be subdir of %s: %w", dir, t.dir, err)
	}
	pkgDir, err := filepath.Rel(t.dir, dir)
	if err != nil {
		return fmt.Errorf("unable to make %s relative to %s: %w", dir, t.dir, err)
	}
	// apply module opts
	for _, o := range bc.opts {
		if err := o(bc); err != nil {
			return fmt.Errorf("unable to configure module %s: %w", bc.modulePath, err)
		}
	}
	// set defaults
	if bc.name == "" {
		bc.name = filepath.Base(dir)
	}
	if bc.out == "" {
		bc.out = bc.name + ".so"
	}
	id, err := PodmanRun(
		ctx,
		conn,
		t,
		QualifiedId(t.pluginbuilderImageId+":"+t.nakamaVersion),
		bc.env,
		append(
			bc.mounts,
			filepath.Join(t.dir)+":/builder",
			filepath.Join(t.volumeDir, "nakama")+":/nakama",
		),
		"go",
		"build",
		"-trimpath",
		"-buildmode=plugin",
		"-o=/nakama/modules/"+bc.out,
		"./"+pkgDir,
	)
	if err != nil {
		return fmt.Errorf("unable to run %s: %w", t.pluginbuilderImageId, err)
	}
	if err := PodmanFollowLogs(ctx, t, id); err != nil {
		return fmt.Errorf("unable to follow logs for %s: %w", id, err)
	}
	if err := PodmanWait(ctx, t, id); err != nil {
		return err
	}
	out := filepath.Join(t.volumeDir, "nakama", "modules", bc.out)
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
func (t *Runner) RunPostgres(ctx, conn context.Context) error {
	var err error
	if t.postgresContainerId, err = PodmanRun(
		ctx,
		conn,
		t,
		QualifiedId(t.postgresImageId+":"+t.postgresVersion),
		map[string]string{
			"listen_addresses":  "'*'",
			"POSTGRES_PASSWORD": t.name,
			"POSTGRES_USER":     t.name,
			"POSTGRES_DB":       t.name,
		},
		[]string{
			filepath.Join(t.volumeDir, "postgres") + ":/var/lib/postgresql/data",
		},
	); err != nil {
		return fmt.Errorf("unable to run %s: %w", t.postgresImageId, err)
	}
	if err := PodmanServiceWait(ctx, t, t.podId, "5432/tcp", func(local, remote string) error {
		t.postgresLocal = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", t.name, t.name, local, t.name)
		t.postgresRemote = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", t.name, t.name, remote, t.name)
		db, err := sql.Open("postgres", t.postgresLocal)
		if err != nil {
			return err
		}
		return db.Ping()
	}); err != nil {
		return fmt.Errorf("unable to connect to postgres %s: %w", t.postgresImageId, err)
	}
	return nil
}

// RunNakama runs the nakama server.
func (t *Runner) RunNakama(ctx, conn context.Context) error {
	var err error
	if t.nakamaContainerId, err = PodmanRun(
		ctx,
		conn,
		t,
		QualifiedId(t.nakamaImageId+":"+t.nakamaVersion),
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
		return fmt.Errorf("unable to run %s: %w", t.nakamaImageId, err)
	}
	if err := PodmanFollowLogs(ctx, t, t.nakamaContainerId); err != nil {
		return fmt.Errorf("unable to follow logs for %s: %w", t.nakamaContainerId, err)
	}
	if err := PodmanServiceWait(ctx, t, t.podId, "7350/tcp", func(local, remote string) error {
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
		return fmt.Errorf("unable to connect to %s (http): %w", t.nakamaImageId, err)
	}
	if err := PodmanServiceWait(ctx, t, t.podId, "7349/tcp", func(local, remote string) error {
		t.grpcLocal = local
		t.grpcRemote = remote
		return nil
	}); err != nil {
		return fmt.Errorf("unable to connect to %s (grpc): %w", t.nakamaImageId, err)
	}
	return nil
}

// HttpClient returns the http client for the test runner.
func (t *Runner) HttpClient() *http.Client {
	return t.httpClient
}

// AlwaysPull returns whether or not to always pull an image.
func (t *Runner) AlwaysPull() bool {
	return t.alwaysPull
}

// DockerRegistryURL returns the docker registry url.
func (t *Runner) DockerRegistryURL() string {
	return t.dockerRegistryURL
}

// DockerTokenURL returns the docker token url.
func (t *Runner) DockerTokenURL() string {
	return t.dockerTokenURL
}

// DockerAuthName returns the docker token auth name.
func (t *Runner) DockerAuthName() string {
	return t.dockerAuthName
}

// DockerAuthScope returns the docker token auth scope for a image id.
func (t *Runner) DockerAuthScope(id string) string {
	return fmt.Sprintf(t.dockerAuthScope, id)
}

// PodId returns the pod id.
func (t *Runner) PodId() string {
	return t.podId
}

// Stdout returns a prefixed writer for output.
func (t *Runner) Stdout(prefix string) io.Writer {
	return NewPrefixedWriter(t.stdout, prefix+": ")
}

// Stderr returns a prefixed writer for errors.
func (t *Runner) Stderr(prefix string) io.Writer {
	return NewPrefixedWriter(t.stderr, prefix+": ")
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

// NetworkRemoveDelay returns the network remove delay.
func (t *Runner) NetworkRemoveDelay() time.Duration {
	return t.networkRemoveDelay
}

// ContainerRemoveDelay returns the container remove delay.
func (t *Runner) ContainerRemoveDelay() time.Duration {
	return t.containerRemoveDelay
}

// Option is a nakama test runner option.
type Option func(*Runner)

// WithName is a nakama test runner option to set the name.
func WithName(name string) Option {
	return func(t *Runner) {
		t.name = name
	}
}

// WithDir is a nakama test runner option to set the project root dir.
func WithDir(dir string) Option {
	return func(t *Runner) {
		t.dir = dir
	}
}

// WithVolumeDir is a nakama test runner option to set the volume dir, where
// nakama and postgres data/configs are written. Default is <project
// root>/.cache. Must be a sub dir of the project root.
func WithVolumeDir(volumeDir string) Option {
	return func(t *Runner) {
		t.volumeDir = volumeDir
	}
}

// WithPostgresImageId is a nakama test runner option to set the postgres image
// id.
func WithPostgresImageId(postgresImageId string) Option {
	return func(t *Runner) {
		t.postgresImageId = postgresImageId
	}
}

// WithNakamaImageId is a nakama test runner option to set the nakama image id.
func WithNakamaImageId(nakamaImageId string) Option {
	return func(t *Runner) {
		t.nakamaImageId = nakamaImageId
	}
}

// WithPluginbuilderImageId is a nakama test runner option to set the
// pluginbuilder image id.
func WithPluginbuilderImageId(pluginbuilderImageId string) Option {
	return func(t *Runner) {
		t.pluginbuilderImageId = pluginbuilderImageId
	}
}

// WithPostgresVersion is a nakama test runner option to set the postgres
// image tag.
func WithPostgresVersion(postgresVersion string) Option {
	return func(t *Runner) {
		t.postgresVersion = postgresVersion
	}
}

// WithNakamaVersion is a nakama test runner option to set the nakama image
// tag.
func WithNakamaVersion(nakamaVersion string) Option {
	return func(t *Runner) {
		t.nakamaVersion = nakamaVersion
	}
}

// WithHttpClient is a nakama test runner option to set a http client used.
// Used for generating auth tokens for image repositories.
func WithHttpClient(httpClient *http.Client) Option {
	return func(t *Runner) {
		t.httpClient = httpClient
	}
}

// WithAlwaysPull is a nakama test runner option to set the always pull flag.
// When true, causes container images to be pulled regardless of if they are
// available on the host or not.
func WithAlwaysPull(alwaysPull bool) Option {
	return func(t *Runner) {
		t.alwaysPull = alwaysPull
	}
}

// WithDockerRegistryURL is a nakama test runner option to set the docker
// registry url used for retrieving images.
func WithDockerRegistryURL(dockerRegistryURL string) Option {
	return func(t *Runner) {
		t.dockerRegistryURL = dockerRegistryURL
	}
}

// WithDockerTokenURL is a nakama test runner option to set the docker token
// url, used for generating auth tokens when pulling images.
func WithDockerTokenURL(dockerTokenURL string) Option {
	return func(t *Runner) {
		t.dockerTokenURL = dockerTokenURL
	}
}

// WithDockerAuthName is a nakama test runner option to set the docker token
// auth name used when generating auth tokens for the docker registry.
func WithDockerAuthName(dockerAuthName string) Option {
	return func(t *Runner) {
		t.dockerAuthName = dockerAuthName
	}
}

// WithDockerAuthScope is a nakama test runner option to set a docker token
// auth scope mask. Must include "%s" to interpolate the image id.
func WithDockerAuthScope(dockerAuthScope string) Option {
	return func(t *Runner) {
		t.dockerAuthScope = dockerAuthScope
	}
}

// WithConfigFilename is a nakama test runner option to set the config
// filename.
func WithConfigFilename(configFilename string) Option {
	return func(t *Runner) {
		t.configFilename = configFilename
	}
}

// WithConfigTemplate is a nakama test runner option to set the config
// template.
func WithConfigTemplate(configTemplate string) Option {
	return func(t *Runner) {
		t.configTemplate = configTemplate
	}
}

// WithNetworkRemoveDelay is a nakama test runner option to set the container
// network remove delay.
func WithNetworkRemoveDelay(networkRemoveDelay time.Duration) Option {
	return func(t *Runner) {
		t.networkRemoveDelay = networkRemoveDelay
	}
}

// WithContainerRemoveDelay is a nakama test runner option to set the container
// remove delay.
func WithContainerRemoveDelay(containerRemoveDelay time.Duration) Option {
	return func(t *Runner) {
		t.containerRemoveDelay = containerRemoveDelay
	}
}

// WithVersionCacheTTL is a nakama test runner option to set the version cache
// TTL.
func WithVersionCacheTTL(versionCacheTTL time.Duration) Option {
	return func(t *Runner) {
		t.versionCacheTTL = versionCacheTTL
	}
}

// WithBackoffMaxInterval is a nakama test runner option to set the backoff max
// interval for waiting for services (ie, postres, nakama) to start/become
// available.
func WithBackoffMaxInterval(backoffMaxInterval time.Duration) Option {
	return func(t *Runner) {
		t.backoffMaxInterval = backoffMaxInterval
	}
}

// WithBackoffMaxElapsedTime is a nakama test runner option to set the backoff
// max elapsed time for waiting for services (ie, postgres, nakama) to
// start/become available.
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

// WithBuildConfig is a nakama test runner option to add a module path, and extra
// options to the build config.
func WithBuildConfig(modulePath string, opts ...BuildConfigOption) Option {
	return func(t *Runner) {
		t.buildConfigs = append(t.buildConfigs, BuildConfig{
			modulePath: modulePath,
			opts:       opts,
		})
	}
}

// BuildConfig is a nakama module build config.
type BuildConfig struct {
	// modulePath is the package build path for the module. Must be sub dir of
	// the working directory.
	modulePath string
	// opts are the module options.
	opts []BuildConfigOption
	// name is the name of the module.
	name string
	// out is the out filename of the module. Will be written to
	// modules/<name>.
	out string
	// env are additional environment variables to pass to
	env map[string]string
	// mounts are additional volume mounts.
	mounts []string
}

// BuildConfigOption is nakama module build config option.
type BuildConfigOption func(*BuildConfig) error

// WithOut is a nakama module build config option to set the out name. When not
// specified, the name will be derived from the directory name of the module.
func WithOut(out string) BuildConfigOption {
	return func(bc *BuildConfig) error {
		bc.out = out
		return nil
	}
}

// WithEnv is a nakama module build config option to set additional env
// variables used during builds.
func WithEnv(env map[string]string) BuildConfigOption {
	return func(bc *BuildConfig) error {
		if bc.env == nil {
			bc.env = make(map[string]string)
		}
		for k, v := range env {
			bc.env[k] = v
		}
		return nil
	}
}

// WithGoEnv is a nakama module build config option to copy the host Go
// environment variables.
func WithGoEnv(env ...string) BuildConfigOption {
	return func(bc *BuildConfig) error {
		if bc.env == nil {
			bc.env = make(map[string]string)
		}
		goPath, err := exec.LookPath("go")
		if err != nil {
			return fmt.Errorf("unable to locate go")
		}
		for _, k := range env {
			v, err := GoEnvVar(goPath, k)
			if err != nil {
				return fmt.Errorf("unable to exec go env %s: %w", k, err)
			}
			bc.env[k] = v
		}
		return nil
	}
}

// WithDefaultGoEnv is a nakama module build config option to copy default host
// environment variables for Go.
//
// Copies:
//	GONOPROXY
//	GONOSUMDB
//	GOPRIVATE
//	GOPROXY
//	GOSUMDB
func WithDefaultGoEnv() BuildConfigOption {
	return WithGoEnv(
		"GONOPROXY",
		"GONOSUMDB",
		"GOPRIVATE",
		"GOPROXY",
		"GOSUMDB",
	)
}

// WithMounts is a nakama module build config option to set additional mounts
// used during builds.
func WithMounts(mounts ...string) BuildConfigOption {
	return func(bc *BuildConfig) error {
		bc.mounts = append(bc.mounts, mounts...)
		return nil
	}
}

// WithGoVolumes is a nakama module build config option to mount the host's Go
// directories (ie, the Go environment's GOCACHE, GOMODCACHE, and GOPATH
// locations) to the plugin builder container. Significantly speeds up build
// times.
//
// Note: use WithDefaultGoVolumes (see below).
func WithGoEnvVolumes(volumes ...EnvVolumeInfo) BuildConfigOption {
	return func(bc *BuildConfig) error {
		goPath, err := exec.LookPath("go")
		if err != nil {
			return fmt.Errorf("unable to locate go")
		}
		for _, vol := range volumes {
			v, err := GoEnvVar(goPath, vol.Key)
			if err != nil {
				return fmt.Errorf("unable to exec go env %s: %w", vol.Key, err)
			}
			if vol.Sub != "" {
				v = filepath.Join(v, vol.Sub)
			}
			v, err = realpath.Realpath(v)
			if err != nil {
				key := vol.Key
				if vol.Sub != "" {
					key += "/" + vol.Sub
				}
				return fmt.Errorf("unable to get real path for go env %s (%s): %w", key, v, err)
			}
			bc.mounts = append(bc.mounts, v+":"+vol.Target)
		}
		return nil
	}
}

// WithGoVolumes is a nakama module build config option to mount the host's Go
// directories (GOCACHE, GOMODCACHE, and GOPATH) to the plugin builder
// container. Significantly speeds up build times.
func WithDefaultGoVolumes() BuildConfigOption {
	return WithGoEnvVolumes(
		NewEnvVolume("GOCACHE", "/root/.cache/go-build", ""),
		NewEnvVolume("GOMODCACHE", "/go/pkg/mod", ""),
		NewEnvVolume("GOPATH", "/go/src", "src"),
	)
}

// EnvVolumeInfo holds information about an environment variable derived
// volume.
type EnvVolumeInfo struct {
	Key    string
	Target string
	Sub    string
}

// NewEnvVolume creates a new environment volume.
func NewEnvVolume(key, target, sub string) EnvVolumeInfo {
	return EnvVolumeInfo{
		Key:    key,
		Target: target,
		Sub:    sub,
	}
}

// PrefixedWriter is a prefixed writer.
type PrefixedWriter struct {
	w      io.Writer
	prefix []byte
}

// NewPrefixedWriter creates a new prefixed writer.
func NewPrefixedWriter(w io.Writer, prefix string) *PrefixedWriter {
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

// ReadCachedFile reads a cached file from disk, returns error if the file name
// on disk is past the ttl.
func ReadCachedFile(name string, ttl time.Duration) ([]byte, error) {
	fi, err := os.Stat(name)
	switch {
	case err != nil:
		return nil, err
	case fi.IsDir():
		return nil, fmt.Errorf("%s is a directory", name)
	case fi.ModTime().Add(ttl).Before(time.Now()):
		return nil, fmt.Errorf("%s needs to be refreshed (past %v)", name, ttl)
	}
	return ioutil.ReadFile(name)
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

// GoEnvVar reads the go env variable from `go env <name>`.
func GoEnvVar(goPath, name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	buf, err := exec.CommandContext(ctx, goPath, "env", name).CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(buf)), nil
}

//go:embed config.yml.tpl
var configYmlTpl []byte
