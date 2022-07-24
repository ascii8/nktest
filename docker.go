package nktest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	dtypes "github.com/docker/docker/api/types"
	dcontainer "github.com/docker/docker/api/types/container"
	dmount "github.com/docker/docker/api/types/mount"
	dnetwork "github.com/docker/docker/api/types/network"
	dclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"github.com/yookoala/realpath"
)

// DockerHandler is a docker handler.
type DockerHandler interface {
	HttpClient() *http.Client
	DockerClient() *dclient.Client
	DockerAuthName() string
	DockerAuthScope(string) string
	DockerRegistryURL() string
	DockerTokenURL() string
	AlwaysPull() bool
	NetworkId() string
	Logf(string, ...interface{})
	Errf(string, ...interface{})
	Stdout() io.Writer
	Stderr() io.Writer
	Backoff(context.Context, func() error) error
}

// GrabDockerImages grabs docker image ids when not present on the docker host
// or when force is true.
func GrabDockerImages(ctx context.Context, h DockerHandler, ids ...string) error {
	cl := h.DockerClient()
	for _, id := range ids {
		// skip if the image exists
		if img, _, err := cl.ImageInspectWithRaw(ctx, id); !h.AlwaysPull() && err == nil {
			h.Logf("%s: %s", id, img.ID)
			continue
		}
		h.Logf("PULLING DOCKER IMAGE: %s", id)
		rc, err := cl.ImagePull(ctx, id, dtypes.ImagePullOptions{})
		if err != nil {
			return err
		}
		defer rc.Close()
		_, _ = io.Copy(h.Stdout(), rc)
	}
	return nil
}

// GetDockerTags gets the tags for a image id.
func GetDockerTags(ctx context.Context, h DockerHandler, id string) ([]string, error) {
	tok, err := GenDockerToken(ctx, h, id)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", h.DockerRegistryURL()+"/v2/"+id+"/tags/list", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", "Bearer "+tok)
	res, err := h.HttpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d != 200", res.StatusCode)
	}
	var v struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	buf, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	_, _ = h.Stdout().Write(append(buf, '\n'))
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v.Tags, nil
}

// GenDockerToken generates a docker auth token for the repo id.
func GenDockerToken(ctx context.Context, h DockerHandler, id string) (string, error) {
	q := url.Values{
		"service": []string{h.DockerAuthName()},
		"scope":   []string{h.DockerAuthScope(id)},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", h.DockerTokenURL()+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	cl := h.HttpClient()
	res, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	buf, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	_, _ = h.Stdout().Write(append(buf, '\n'))
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	var v struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		IssuedAt    string `json:"issued_at"`
	}
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d != 200", res.StatusCode)
	}
	if v.Token == "" {
		return "", fmt.Errorf("empty token for %s", id)
	}
	return v.Token, nil
}

// CreateDockerNetwork creates a docker network.
func CreateDockerNetwork(ctx context.Context, h DockerHandler, name string, removeDelay time.Duration) (string, error) {
	cl := h.DockerClient()
	if res, err := cl.NetworkInspect(ctx, name, dtypes.NetworkInspectOptions{}); err == nil {
		return res.ID, nil
	}
	res, err := cl.NetworkCreate(ctx, name, dtypes.NetworkCreate{})
	if err != nil {
		return "", err
	}
	go func() {
		<-ctx.Done()
		<-time.After(removeDelay)
		h.Logf("REMOVING NETWORK: %s", res.ID)
		if err := cl.NetworkRemove(context.Background(), res.ID); err != nil && !dclient.IsErrNotFound(err) {
			h.Errf("ERROR: unable to remove network %s: %v", res.ID, err)
		}
	}()
	return res.ID, nil
}

// DockerRun runs a docker image id.
func DockerRun(ctx context.Context, h DockerHandler, id string, removeDelay time.Duration, env []string, mounts []string, entrypoint ...string) (string, error) {
	m, err := DockerBuildMounts(mounts...)
	if err != nil {
		return "", err
	}
	h.Logf("RUNNING: %s", id)
	cl := h.DockerClient()
	res, err := cl.ContainerCreate(ctx, &dcontainer.Config{
		Image:        id,
		AttachStdout: true,
		AttachStderr: true,
		Env:          env,
		Entrypoint:   entrypoint,
	}, &dcontainer.HostConfig{
		AutoRemove:      true,
		PublishAllPorts: true,
		Mounts:          m,
	}, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("unable to create %s: %w", id, err)
	}
	h.Logf("CREATED %s: %s", id, res.ID)
	go func() {
		<-ctx.Done()
		h.Logf("STOPPING %s: %s", id, res.ID)
		if err := cl.ContainerStop(context.Background(), res.ID, &removeDelay); err != nil && !dclient.IsErrNotFound(err) {
			h.Errf("ERROR STOPPING %s %s: %v", id, res.ID, err)
		}
	}()
	if networkId := h.NetworkId(); networkId != "" {
		if err := cl.NetworkConnect(ctx, networkId, res.ID, &dnetwork.EndpointSettings{}); err != nil {
			return "", fmt.Errorf("unable to connect %s to network %s: %w", id, networkId, err)
		}
	}
	if err := cl.ContainerStart(ctx, res.ID, dtypes.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("unable to start postgres: %w", err)
	}
	h.Logf("STARTED %s: %s", id, res.ID)
	return res.ID, nil
}

// DockerBuildMounts creates mounts for docker.
func DockerBuildMounts(mounts ...string) ([]dmount.Mount, error) {
	m := make([]dmount.Mount, len(mounts))
	for i := 0; i < len(mounts); i++ {
		j := strings.LastIndex(mounts[i], ":")
		if j == -1 {
			return nil, fmt.Errorf("mount %d %q missing ':'", i, mounts[i])
		}
		s, err := realpath.Realpath(mounts[i][:j])
		if err != nil {
			return nil, fmt.Errorf("invalid path: %w", err)
		}
		m[i] = dmount.Mount{
			Type:   dmount.TypeBind,
			Source: s,
			Target: mounts[i][j+1:],
		}
	}
	return m, nil
}

// DockerWait waits until a container has exited.
func DockerWait(ctx context.Context, h DockerHandler, id string) error {
	// wait for container to exit
	statusCh, errCh := h.DockerClient().ContainerWait(ctx, id, dcontainer.WaitConditionNotRunning)
	select {
	case <-ctx.Done():
	case <-statusCh:
	case err := <-errCh:
		return err
	}
	return nil
}

// DockerServiceWait waits for a docker container service to be available.
func DockerServiceWait(ctx context.Context, h DockerHandler, id, svc string, f func(string, string) error) error {
	local, remote, err := GetDockerAddr(ctx, h, id, svc)
	if err != nil {
		return err
	}
	return h.Backoff(ctx, func() error {
		return f(local, remote)
	})
}

// GetDockerAddr inspects id and returns the local and remote addresses.
func GetDockerAddr(ctx context.Context, h DockerHandler, id, svc string) (string, string, error) {
	res, err := h.DockerClient().ContainerInspect(ctx, id)
	if err != nil {
		return "", "", err
	}
	p := nat.Port(svc)
	port, ok := res.NetworkSettings.Ports[p]
	if !ok || len(port) == 0 {
		return "", "", fmt.Errorf("container %s does not have service %q", id, svc)
	}
	localIP := port[0].HostIP
	if localIP == "0.0.0.0" {
		localIP = "127.0.0.1"
	}
	return localIP + ":" + port[0].HostPort, res.NetworkSettings.IPAddress + ":" + p.Port(), nil
}

// DockerFollowLogs follows the logs for a container.
func DockerFollowLogs(ctx context.Context, h DockerHandler, id string) error {
	cl := h.DockerClient()
	logs, err := cl.ContainerLogs(ctx, id, dtypes.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return fmt.Errorf("unable to get logs for %s: %w", id, err)
	}
	go func() {
		_, _ = stdcopy.StdCopy(h.Stdout(), h.Stderr(), logs)
	}()
	return nil
}
