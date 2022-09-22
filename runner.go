package nktest

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	_ "github.com/lib/pq"
	"github.com/yookoala/realpath"
)

// Runner is a nakama test runner.
type Runner struct {
	// name is the name pod.
	name string
	// dir is the working directory.
	dir string
	// volumeDir is the directory for container volumes.
	volumeDir string
	// buildConfigs are the module build configs.
	buildConfigs []BuildConfig
	// podId is the created pod id.
	podId string
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
	// consoleLocal is the local console address.
	consoleLocal string
	// consoleRemote is the remote console address.
	consoleRemote string
}

// NewRunner creates a new nakama test runner.
func NewRunner(opts ...Option) *Runner {
	t := new(Runner)
	for _, o := range opts {
		o(t)
	}
	return t
}

// Run handles building the nakama plugin and starting the postgres and
// nakama server containers.
func (t *Runner) Run(ctx context.Context) error {
	// setup project working directory
	if err := t.init(ctx); err != nil {
		return err
	}
	// get the podman context
	ctx, err := PodmanOpen(ctx)
	if err != nil {
		return fmt.Errorf("unable to create podman client: %w", err)
	}
	// images
	postgresImageId, pluginbuilderImageId, nakamaImageId := PostgresImageId(ctx), PluginbuilderImageId(ctx), NakamaImageId(ctx)
	// versions
	postgresVersion := PostgresVersion(ctx)
	nakamaVersion, err := NakamaVersion(ctx)
	if err != nil {
		return err
	}
	Logf(ctx, "% 16s: %s", "NAKAMA VERSION", nakamaVersion)
	qualifiedPostgresId := QualifiedId(postgresImageId + ":" + postgresVersion)
	qualifiedPluginbuilderId := QualifiedId(pluginbuilderImageId + ":" + nakamaVersion)
	qualifiedNakamaId := QualifiedId(nakamaImageId + ":" + nakamaVersion)
	// retrieve images
	if err := PodmanPullImages(
		ctx,
		qualifiedPostgresId,
		qualifiedPluginbuilderId,
		qualifiedNakamaId,
	); err != nil {
		return fmt.Errorf("unable to retrieve images: %w", err)
	}
	// build modules
	if err := t.BuildModules(ctx, qualifiedPluginbuilderId); err != nil {
		return fmt.Errorf("unable to build modules: %w", err)
	}
	// create network for images
	if t.podId, err = PodmanCreatePod(
		ctx,
		t.name,
		QualifiedId(postgresImageId+":"+postgresVersion),
		QualifiedId(nakamaImageId+":"+nakamaVersion),
	); err != nil {
		return fmt.Errorf("unable to create pod: %w", err)
	}
	// run postgres
	if err := t.RunPostgres(ctx, qualifiedPostgresId); err != nil {
		return fmt.Errorf("unable to start postgres: %w", err)
	}
	// run nakama
	if err := t.RunNakama(ctx, qualifiedNakamaId); err != nil {
		return fmt.Errorf("unable to start nakama: %w", err)
	}
	return nil
}

// init initializes the environment for running nakama-pluginbuilder and nakama
// images.
func (t *Runner) init(ctx context.Context) error {
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
	tpl, err := template.New("").Parse(ConfigTemplate(ctx))
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
	configFilename := ConfigFilename(ctx)
	if err := os.WriteFile(filepath.Join(t.volumeDir, "nakama", configFilename), buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("unable to write %s: %w", configFilename, err)
	}
	return nil
}

// BuildModules builds the nakama modules.
func (t *Runner) BuildModules(ctx context.Context, id string) error {
	for i, bc := range t.buildConfigs {
		if err := t.BuildModule(ctx, id, &bc); err != nil {
			return fmt.Errorf("unable to build module %d: %w", i, err)
		}
	}
	return nil
}

// BuildModule builds a nakama plugin module.
func (t *Runner) BuildModule(ctx context.Context, id string, bc *BuildConfig) error {
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
	containerId, err := PodmanRun(
		ctx,
		t.podId,
		id,
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
		return fmt.Errorf("unable to run %s: %w", id, err)
	}
	if err := PodmanFollowLogs(ctx, containerId); err != nil {
		return fmt.Errorf("unable to follow logs for %s: %w", ShortId(containerId), err)
	}
	if err := PodmanWait(ctx, containerId); err != nil {
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
	Logf(ctx, "% 16s: %s %d", "BUILT", out, fi.Size())
	return nil
}

// RunPostgres runs the postgres server.
func (t *Runner) RunPostgres(ctx context.Context, id string) error {
	containerId, err := PodmanRun(
		ctx,
		t.podId,
		id,
		map[string]string{
			"listen_addresses":  "'*'",
			"POSTGRES_PASSWORD": t.name,
			"POSTGRES_USER":     t.name,
			"POSTGRES_DB":       t.name,
		},
		[]string{
			filepath.Join(t.volumeDir, "postgres") + ":/var/lib/postgresql/data",
		},
	)
	if err != nil {
		return fmt.Errorf("unable to run %s: %w", id, err)
	}
	if err := PodmanServiceWait(ctx, t.podId, "5432/tcp", func(local, remote string) error {
		t.postgresLocal = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", t.name, t.name, local, t.name)
		t.postgresRemote = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", t.name, t.name, remote, t.name)
		db, err := sql.Open("postgres", t.postgresLocal)
		if err != nil {
			return err
		}
		return db.Ping()
	}); err != nil {
		return fmt.Errorf("unable to connect to postgres %s: %w", ShortId(containerId), err)
	}
	return nil
}

// RunNakama runs the nakama server.
func (t *Runner) RunNakama(ctx context.Context, id string) error {
	containerId, err := PodmanRun(
		ctx,
		t.podId,
		id,
		nil,
		[]string{
			filepath.Join(t.volumeDir, "nakama") + ":/nakama/data",
		},
		`/bin/sh`,
		`-ecx`,
		`/nakama/nakama migrate up `+
			`--database.address=`+t.postgresRemote+` && `+
			`exec /nakama/nakama `+
			`--config=/nakama/data/config.yml `+
			`--database.address=`+t.postgresRemote,
	)
	if err != nil {
		return fmt.Errorf("unable to run %s: %w", id, err)
	}
	// follow logs
	if err := PodmanFollowLogs(ctx, containerId); err != nil {
		return fmt.Errorf("unable to follow logs for %s: %w", ShortId(containerId), err)
	}
	// wait for http to be available
	if err := PodmanServiceWait(ctx, t.podId, "7350/tcp", func(local, remote string) error {
		t.httpLocal = "http://" + local
		t.httpRemote = "http://" + remote
		req, err := http.NewRequestWithContext(ctx, "GET", t.httpLocal+"/healthcheck", nil)
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
		return fmt.Errorf("unable to connect to %s (http): %w", ShortId(containerId), err)
	}
	// grpc ports
	if err := PodmanServiceWait(ctx, t.podId, "7349/tcp", func(local, remote string) error {
		t.grpcLocal = local
		t.grpcRemote = remote
		return nil
	}); err != nil {
		return fmt.Errorf("unable to connect to %s (grpc): %w", ShortId(containerId), err)
	}
	// console ports
	if err := PodmanServiceWait(ctx, t.podId, "7351/tcp", func(local, remote string) error {
		prefix := "http://" + t.name + ":" + t.name + "_password@"
		t.consoleLocal = prefix + local
		t.consoleRemote = prefix + remote
		return nil
	}); err != nil {
		return fmt.Errorf("unable to connect to %s (console): %w", ShortId(containerId), err)
	}
	return nil
}

// RunProxy creates and runs a http proxy until the context is closed.
func (t *Runner) RunProxy(ctx context.Context, opts ...ProxyOption) (string, error) {
	return NewProxy(opts...).Run(ctx, t.httpLocal)
}

// PodId returns the pod id.
func (t *Runner) PodId() string {
	return t.podId
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

// ConsoleLocal returns the console local address.
func (t *Runner) ConsoleLocal() string {
	return t.consoleLocal
}

// ConsoleRemote returns the console remote address.
func (t *Runner) ConsoleRemote() string {
	return t.consoleRemote
}

// Name returns the name.
func (t *Runner) Name() string {
	return t.name
}

// HttpKey returns the http key.
func (t *Runner) HttpKey() string {
	return t.name
}

// ServerKey returns the server key.
func (t *Runner) ServerKey() string {
	return t.name + "_server"
}

// Option is a nakama test runner option.
type Option func(*Runner)

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
//
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
			if err := os.MkdirAll(v, 0o755); err != nil {
				return fmt.Errorf("unable to mkdir: %w", err)
			}
			if v, err = realpath.Realpath(v); err != nil {
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
	return os.ReadFile(name)
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
