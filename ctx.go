package nktest

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"golang.org/x/mod/semver"
)

// Defaults.
var (
	DefaultPrefixOut             = "-> "
	DefaultPrefixIn              = "<- "
	DefaultAlwaysPull            = false
	DefaultPostgresImageId       = "docker.io/library/postgres"
	DefaultNakamaImageId         = "docker.io/heroiclabs/nakama"
	DefaultPluginbuilderImageId  = "docker.io/heroiclabs/nakama-pluginbuilder"
	DefaultPostgresVersion       = "latest"
	DefaultDockerRegistryURL     = "https://registry-1.docker.io"
	DefaultDockerTokenURL        = "https://auth.docker.io/token"
	DefaultDockerAuthName        = "registry.docker.io"
	DefaultDockerAuthScope       = "repository:%s:pull"
	DefaultVersionCacheTTL       = 96 * time.Hour
	DefaultPodRemoveTimeout      = 200 * time.Millisecond
	DefaultBackoffMaxInterval    = 2 * time.Second
	DefaultBackoffMaxElapsedTime = 30 * time.Second
	DefaultConfigFilename        = "config.yml"
	//go:embed config.yml.tpl
	DefaultConfigTemplate string
)

// contextKey is a context key.
type contextKey int

// context keys.
const (
	stdoutKey contextKey = iota
	loggerKey
	coutKey
	httpClientKey
	podmanConnKey
	portMapKey
	alwaysPullKey
	dockerRegistryURLKey
	dockerTokenURLKey
	dockerAuthNameKey
	dockerAuthScopeKey
	postgresImageIdKey
	nakamaImageIdKey
	pluginbuilderImageIdKey
	postgresVersionKey
	nakamaVersionKey
	configFilenameKey
	configTemplateKey
	versionCacheTTLKey
	podRemoveTimeoutKey
	backoffMaxIntervalKey
	backoffMaxElapsedTimeKey
)

// WithStdout sets the stdout on the context.
func WithStdout(parent context.Context, stdout io.Writer) context.Context {
	return context.WithValue(parent, stdoutKey, stdout)
}

/*
// WithStderr sets the stderr on the context.
func WithStderr(parent context.Context, stderr io.Writer) context.Context {
	return context.WithValue(parent, stderrKey, stderr)
}
*/

// WithCout sets the console writer out on the context.
func WithCout(parent context.Context, cout io.Writer) context.Context {
	return context.WithValue(parent, coutKey, cout)
}

/*
// WithCerr sets the console writer err on the context.
func WithCerr(parent context.Context, cerr io.Writer) context.Context {
	return context.WithValue(parent, cerrKey, cerr)
}
*/

// WithHttpClient sets the http client used on the context. Used for generating
// auth tokens for image repositories.
func WithHttpClient(parent context.Context, httpClient *http.Client) context.Context {
	return context.WithValue(parent, httpClientKey, httpClient)
}

// WithPodmanConn sets the podman conn used on the context.
func WithPodmanConn(parent, conn context.Context) context.Context {
	return context.WithValue(parent, podmanConnKey, conn)
}

// WithPortMap adds a host port mapping for a service to the context.
func WithPortMap(parent context.Context, id, svc string, port uint16) context.Context {
	ctx := parent
	portMap, ok := ctx.Value(portMapKey).(map[string]uint16)
	if !ok {
		portMap = make(map[string]uint16)
		ctx = context.WithValue(ctx, portMapKey, portMap)
	}
	if id != "" {
		portMap[QualifiedId(id)+":"+svc] = port
	} else {
		portMap[svc] = port
	}
	return ctx
}

// WithHostPortMap adds host port mappings for the postgres and nakama services
// (5432/tcp, 7349/tcp, 7350/tcp, 7351/tcp) to the context.
func WithHostPortMap(parent context.Context) context.Context {
	ctx := parent
	ctx = WithPortMap(ctx, "postgres", "5432/tcp", 5432)
	ctx = WithPortMap(ctx, "heroiclabs/nakama", "7349/tcp", 7349)
	ctx = WithPortMap(ctx, "heroiclabs/nakama", "7350/tcp", 7350)
	ctx = WithPortMap(ctx, "heroiclabs/nakama", "7351/tcp", 7351)
	return ctx
}

// WithAlwaysPull sets the always pull flag on the context. When true, causes
// container images to be pulled regardless of if they are available on the
// host or not.
func WithAlwaysPull(parent context.Context, alwaysPull bool) context.Context {
	return context.WithValue(parent, alwaysPullKey, alwaysPull)
}

// WithAlwaysPull sets the always pull flag from an environment variable on the
// context.
func WithAlwaysPullFromEnv(parent context.Context, name string) context.Context {
	pull := os.Getenv(name)
	return context.WithValue(parent, alwaysPullKey, pull != "" && pull != "false" && pull != "0")
}

// WithDockerRegistryURL sets the docker registry url on the context. Used for
// retrieving images.
func WithDockerRegistryURL(parent context.Context, dockerRegistryURL string) context.Context {
	return context.WithValue(parent, dockerRegistryURLKey, dockerRegistryURL)
}

// WithDockerTokenURL sets the docker token url on the context. Used for
// generating auth tokens when pulling images.
func WithDockerTokenURL(parent context.Context, dockerTokenURL string) context.Context {
	return context.WithValue(parent, dockerTokenURLKey, dockerTokenURL)
}

// WithDockerAuthName sets the docker token auth name on the context. Used when
// generating auth tokens for the docker registry.
func WithDockerAuthName(parent context.Context, dockerAuthName string) context.Context {
	return context.WithValue(parent, dockerAuthNameKey, dockerAuthName)
}

// WithDockerAuthScope sets a docker token auth scope mask on the context. Must
// include "%s" to interpolate the image id.
func WithDockerAuthScope(parent context.Context, dockerAuthScope string) context.Context {
	return context.WithValue(parent, dockerAuthScopeKey, dockerAuthScope)
}

// WithPostgresImageId sets the postgres image id on the context.
func WithPostgresImageId(parent context.Context, postgresImageId string) context.Context {
	return context.WithValue(parent, postgresImageIdKey, postgresImageId)
}

// WithNakamaImageId sets the nakama image id on the context.
func WithNakamaImageId(parent context.Context, nakamaImageId string) context.Context {
	return context.WithValue(parent, nakamaImageIdKey, nakamaImageId)
}

// WithPluginbuilderImageId sets the pluginbuilder image id on the context.
func WithPluginbuilderImageId(parent context.Context, pluginbuilderImageId string) context.Context {
	return context.WithValue(parent, pluginbuilderImageIdKey, pluginbuilderImageId)
}

// WithPostgresVersion sets the postgres image tag on the context.
func WithPostgresVersion(parent context.Context, postgresVersion string) context.Context {
	return context.WithValue(parent, postgresVersionKey, postgresVersion)
}

// WithNakamaVersion sets the nakama image tag on the context.
func WithNakamaVersion(parent context.Context, nakamaVersion string) context.Context {
	return context.WithValue(parent, nakamaVersionKey, nakamaVersion)
}

// WithConfigFilename sets the config filename on the context.
func WithConfigFilename(parent context.Context, configFilename string) context.Context {
	return context.WithValue(parent, configFilenameKey, configFilename)
}

// WithConfigTemplate sets the config template on the context.
func WithConfigTemplate(parent context.Context, configTemplate string) context.Context {
	return context.WithValue(parent, configTemplateKey, configTemplate)
}

// WithVersionCacheTTL sets the version cache TTL on the context.
func WithVersionCacheTTL(parent context.Context, versionCacheTTL time.Duration) context.Context {
	return context.WithValue(parent, versionCacheTTLKey, versionCacheTTL)
}

// WithPodRemoveTimeout sets the pod remove timeout on the context.
func WithPodRemoveTimeout(parent context.Context, podRemoveTimeout time.Duration) context.Context {
	return context.WithValue(parent, podRemoveTimeoutKey, podRemoveTimeout)
}

// WithBackofffMaxInterval sets the max backoff interval on the context. Used
// when waiting for services (ie, postres, nakama) to start/become available.
func WithBackoffMaxInterval(parent context.Context, maxInterval time.Duration) context.Context {
	return context.WithValue(parent, backoffMaxIntervalKey, maxInterval)
}

// WithBackofffMaxElapsedTime sets the max backoff elapsed time on the context.
// Used when waiting for services (ie, postgres, nakama) to start/become
// available.
func WithBackoffMaxElapsedTime(parent context.Context, maxElapsedTime time.Duration) context.Context {
	return context.WithValue(parent, backoffMaxElapsedTimeKey, maxElapsedTime)
}

// PortMap returns the port map from the context.
func PortMap(ctx context.Context) map[string]uint16 {
	if portMap, ok := ctx.Value(portMapKey).(map[string]uint16); ok && portMap != nil {
		return portMap
	}
	return map[string]uint16{}
}

// HostPortMap returns the host port for the provided container id and service from the context.
func HostPortMap(ctx context.Context, id, svc string, containerPort, hostPort uint16) uint16 {
	portMap := PortMap(ctx)
	ids := []string{id}
	if i := strings.LastIndex(id, ":"); i != -1 {
		ids = append(ids, id[:i])
	}
	for _, s := range ids {
		if p, ok := portMap[s+":"+svc]; ok {
			return p
		}
	}
	if p, ok := portMap[svc]; ok {
		return p
	}
	return hostPort
}

// PodmanConn returns the podman connection on the context.
func PodmanConn(ctx context.Context) context.Context {
	if conn, ok := ctx.Value(podmanConnKey).(context.Context); ok && conn != nil {
		return conn
	}
	return ctx
}

// AlwaysPull returns whether or not to always pull an image.
func AlwaysPull(ctx context.Context) bool {
	if alwaysPull, ok := ctx.Value(alwaysPullKey).(bool); ok {
		return alwaysPull
	}
	return DefaultAlwaysPull
}

// DockerRegistryURL returns the docker registry url.
func DockerRegistryURL(ctx context.Context) string {
	if dockerRegistryURL, ok := ctx.Value(dockerRegistryURLKey).(string); ok {
		return dockerRegistryURL
	}
	return DefaultDockerRegistryURL
}

// DockerTokenURL returns the docker token url.
func DockerTokenURL(ctx context.Context) string {
	if dockerTokenURL, ok := ctx.Value(dockerTokenURLKey).(string); ok {
		return dockerTokenURL
	}
	return DefaultDockerTokenURL
}

// DockerAuthName returns the docker token auth name.
func DockerAuthName(ctx context.Context) string {
	if dockerAuthName, ok := ctx.Value(dockerAuthNameKey).(string); ok {
		return dockerAuthName
	}
	return DefaultDockerAuthName
}

// DockerAuthScope returns the docker token auth scope for a image id.
func DockerAuthScope(ctx context.Context, id string) string {
	dockerAuthScope := DefaultDockerAuthScope
	if s, ok := ctx.Value(dockerAuthNameKey).(string); ok {
		dockerAuthScope = s
	}
	return fmt.Sprintf(dockerAuthScope, id)
}

// PostgresVersion returns the postgres version.
func PostgresVersion(ctx context.Context) string {
	if postgresVersion, ok := ctx.Value(postgresVersionKey).(string); ok {
		return postgresVersion
	}
	return DefaultPostgresVersion
}

// NakamaImageId returns the nakama image id.
func NakamaImageId(ctx context.Context) string {
	if nakamaImageId, ok := ctx.Value(nakamaImageIdKey).(string); ok {
		return nakamaImageId
	}
	return DefaultNakamaImageId
}

// PluginbuilderImageId returns the pluginbuilder image id.
func PluginbuilderImageId(ctx context.Context) string {
	if pluginbuilderImageId, ok := ctx.Value(pluginbuilderImageIdKey).(string); ok {
		return pluginbuilderImageId
	}
	return DefaultPluginbuilderImageId
}

// PostgresImageId returns the postgres image id.
func PostgresImageId(ctx context.Context) string {
	if postgresImageId, ok := ctx.Value(postgresImageIdKey).(string); ok {
		return postgresImageId
	}
	return DefaultPostgresImageId
}

// NakamaVersion loads and caches the nakama version.
func NakamaVersion(ctx context.Context) (string, error) {
	if ver, _ := ctx.Value(nakamaVersionKey).(string); ver != "" {
		return ver, nil
	}
	// get user cache storage
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("unable to get user cache dir: %w", err)
	}
	// create cache dir
	cacheDir = filepath.Join(cacheDir, "nktest")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("unable to create cache dir %s: %w", cacheDir, err)
	}
	cacheFile := filepath.Join(cacheDir, "nakama-version")
	// read cached version
	if ver, err := ReadCachedFile(cacheFile, VersionCacheTTL(ctx)); err == nil {
		return string(bytes.TrimSpace(ver)), nil
	}
	nakamaImageId, pluginbuilderImageId := NakamaImageId(ctx), PluginbuilderImageId(ctx)
	Info(ctx).Str("nakama", nakamaImageId).Str("pluginbuilder", pluginbuilderImageId).Msg("refreshing")
	// get nakama versions
	nk, err := DockerImageTags(ctx, nakamaImageId)
	switch {
	case err != nil:
		return "", fmt.Errorf("unable to get tags for %s: %w", nakamaImageId, err)
	case len(nk) == 0:
		return "", fmt.Errorf("no tags available for %s", nakamaImageId)
	}
	// get pluginbuilder versions
	pb, err := DockerImageTags(ctx, pluginbuilderImageId)
	switch {
	case err != nil:
		return "", fmt.Errorf("unable to get tags for %s: %w", pluginbuilderImageId, err)
	case len(nk) == 0:
		return "", fmt.Errorf("no tags available for %s", pluginbuilderImageId)
	}
	Info(ctx).Strs("nakama", nk).Strs("pluginbuilder", pb).Msg("available")
	// create map of pluginbuilder versions
	m, re := make(map[string]bool), regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
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
	// determine most recent pluginbuilder version matching available nakama version
	for i := len(v) - 1; i >= 0; i-- {
		ver := strings.TrimPrefix(v[i], "v")
		if !m[ver] {
			continue
		}
		if err := os.WriteFile(cacheFile, []byte(ver+"\n"), 0o644); err != nil {
			return "", fmt.Errorf("unable to write %s: %w", cacheFile, err)
		}
		return ver, nil
	}
	return "", fmt.Errorf("no available version of %s matches available versions for %s", pluginbuilderImageId, nakamaImageId)
}

// VersionCacheTTL returns the version cache ttl.
func VersionCacheTTL(ctx context.Context) time.Duration {
	if versionCacheTTL, ok := ctx.Value(versionCacheTTLKey).(time.Duration); ok {
		return versionCacheTTL
	}
	return DefaultVersionCacheTTL
}

// PodRemoveTimeout returns the pod remove timeout.
func PodRemoveTimeout(ctx context.Context) time.Duration {
	if podRemoveTimeout, ok := ctx.Value(podRemoveTimeoutKey).(time.Duration); ok {
		return podRemoveTimeout
	}
	return DefaultPodRemoveTimeout
}

// Backoff executes f until backoff conditions are met or until f returns nil,
// or the context is closed.
func Backoff(ctx context.Context, f func() error) error {
	p := backoff.NewExponentialBackOff()
	p.MaxInterval = DefaultBackoffMaxInterval
	if d, ok := ctx.Value(backoffMaxIntervalKey).(time.Duration); ok {
		p.MaxInterval = d
	}
	p.MaxElapsedTime = DefaultBackoffMaxElapsedTime
	if d, ok := ctx.Value(backoffMaxElapsedTimeKey).(time.Duration); ok {
		p.MaxElapsedTime = d
	}
	return backoff.Retry(f, backoff.WithContext(p, ctx))
}

// ConfigTemplate returns the config template.
func ConfigTemplate(ctx context.Context) string {
	if configTemplate, ok := ctx.Value(configTemplateKey).(string); ok {
		return configTemplate
	}
	return DefaultConfigTemplate
}

// ConfigFilename returns the config filename.
func ConfigFilename(ctx context.Context) string {
	if configFilename, ok := ctx.Value(configFilenameKey).(string); ok {
		return configFilename
	}
	return DefaultConfigFilename
}
