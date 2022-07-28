package nktest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	pntypes "github.com/containers/common/libnetwork/types"
	pdefine "github.com/containers/podman/v4/libpod/define"
	pbindings "github.com/containers/podman/v4/pkg/bindings"
	pcontainers "github.com/containers/podman/v4/pkg/bindings/containers"
	pimages "github.com/containers/podman/v4/pkg/bindings/images"
	ppods "github.com/containers/podman/v4/pkg/bindings/pods"
	pentities "github.com/containers/podman/v4/pkg/domain/entities"
	perrors "github.com/containers/podman/v4/pkg/errorhandling"
	pspecgen "github.com/containers/podman/v4/pkg/specgen"
	pspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/teivah/onecontext"

	"github.com/yookoala/realpath"
)

// Handler is a handler.
type Handler interface {
	Name() string
	HttpClient() *http.Client
	DockerAuthName() string
	DockerAuthScope(string) string
	DockerRegistryURL() string
	DockerTokenURL() string
	AlwaysPull() bool
	PodId() string
	Stdout(string) io.Writer
	Stderr(string) io.Writer
	Logf(string, ...interface{})
	Errf(string, ...interface{})
	Backoff(context.Context, func() error) error
	ContainerRemoveDelay() time.Duration
	NetworkRemoveDelay() time.Duration
}

// PodmanContext creates a podman client.
func PodmanContext(ctx context.Context) (context.Context, context.Context, error) {
	// no podman client was passed with the context
	if _, err := pbindings.GetClient(ctx); err != nil {
		// TODO: check this works on windows + macos properly
		dir := os.Getenv("XDG_RUNTIME_DIR")
		if dir == "" {
			dir = "/var/run"
		}
		conn, err := pbindings.NewConnection(context.Background(), "unix:"+dir+"/podman/podman.sock")
		if err != nil {
			return nil, nil, err
		}
		ctx, _ := onecontext.Merge(ctx, conn)
		return ctx, conn, nil
	}
	return ctx, ctx, nil
}

// PodmanPullImages grabs image ids when not present on the host or when the
// handler's always pull is true.
func PodmanPullImages(ctx, conn context.Context, h Handler, ids ...string) error {
	for _, id := range ids {
		// skip if the image exists
		if img, err := pimages.GetImage(ctx, id, nil); err == nil && !h.AlwaysPull() {
			h.Logf("EXISTING: %s %s", id, ShortId(img.ID))
			continue
		}
		h.Logf("PULLING: %s", id)
		if _, err := pimages.Pull(ctx, id, nil); err != nil {
			return err
		}
	}
	return nil
}

// PodmanCreatePod creates a pod network.
func PodmanCreatePod(ctx, conn context.Context, h Handler, ids ...string) (string, error) {
	name := h.Name()
	// inspect containder ids and get ports to publish
	var portMappings []pntypes.PortMapping
	for _, id := range ids {
		img, err := pimages.GetImage(ctx, id, nil)
		if err != nil {
			return "", fmt.Errorf("unable to inspect image %s: %w", id, err)
		}
		for k := range img.Config.ExposedPorts {
			port, err := NewPortMapping(k)
			if err != nil {
				return "", fmt.Errorf("image %s has invalid service definition %q: %w", id, k, err)
			}
			portMappings = append(portMappings, port)
		}
	}
	// create spec
	var err error
	spec := pspecgen.NewPodSpecGenerator()
	spec.InfraName = name
	if spec, err = pentities.ToPodSpecGen(*spec, &pentities.PodCreateOptions{
		Name:  name,
		Infra: true,
		Net: &pentities.NetOptions{
			PublishPorts: portMappings,
		},
		Userns: pspecgen.Namespace{NSMode: pspecgen.KeepID},
	}); err != nil {
		return "", fmt.Errorf("unable to create network pod spec: %w", err)
	}
	// create pod
	res, err := ppods.CreatePodFromSpec(ctx, &pentities.PodSpec{
		PodSpecGen: *spec,
	})
	if err != nil {
		return "", fmt.Errorf("unable to create network pod: %w", err)
	}
	go func() {
		<-ctx.Done()
		<-time.After(h.NetworkRemoveDelay())
		h.Logf("REMOVING POD: %s %s", name, ShortId(res.Id))
		if _, err := ppods.Remove(conn, res.Id, nil); err != nil {
			h.Errf("unable to remove pod %s %s: %v", name, ShortId(res.Id), err)
		}
	}()
	return res.Id, nil
}

// PodmanRun runs a container image id.
func PodmanRun(ctx, conn context.Context, h Handler, id string, env map[string]string, mounts []string, entrypoint ...string) (string, error) {
	h.Logf("RUN: %s", id)
	// create spec
	s := pspecgen.NewSpecGenerator(id, false)
	s.Remove = true
	s.Entrypoint = entrypoint
	s.Env = env
	if podId := h.PodId(); podId != "" {
		s.Pod = podId
	}
	var err error
	if s.Mounts, err = PodmanBuildMounts(mounts...); err != nil {
		return "", err
	}
	// create
	res, err := pcontainers.CreateWithSpec(ctx, s, nil)
	if err != nil {
		return "", fmt.Errorf("unable to create %s: %w", id, err)
	}
	h.Logf("CREATED: %s %s", id, ShortId(res.ID))
	go func() {
		<-ctx.Done()
		h.Logf("STOPPING: %s %s", id, ShortId(res.ID))
		opts := new(pcontainers.StopOptions).WithTimeout(uint(h.ContainerRemoveDelay().Seconds()))
		if err := pcontainers.Stop(conn, res.ID, opts); err != nil && !perrors.Contains(err, pdefine.ErrNoSuchCtr) {
			h.Errf("unable to stop container %s %s: %v", id, ShortId(res.ID), err)
		}
	}()
	// run
	if err := pcontainers.Start(ctx, res.ID, nil); err != nil {
		return "", fmt.Errorf("unable to start %s %s: %w", id, ShortId(res.ID), err)
	}
	h.Logf("RUNNING: %s %s", id, ShortId(res.ID))
	return res.ID, nil
}

// PodmanFollowLogs follows the logs for a container.
func PodmanFollowLogs(ctx context.Context, h Handler, id string) error {
	go func() {
		shortId := ShortId(id)
		if err := pcontainers.Attach(
			ctx,
			id,
			nil,
			h.Stdout(shortId+": "),
			h.Stderr(shortId+": "),
			nil,
			&pcontainers.AttachOptions{},
		); err != nil {
			h.Errf("unable to get logs for %s: %v", shortId, err)
		}
	}()
	return nil
}

// PodmanWait waits until a container has stopped.
func PodmanWait(ctx context.Context, h Handler, id string) error {
	if _, err := pcontainers.Wait(ctx, id, &pcontainers.WaitOptions{
		Condition: []pdefine.ContainerStatus{
			pdefine.ContainerStateStopped,
		},
	}); err != nil {
		return fmt.Errorf("unable to wait for container %s to stop: %w", id, err)
	}
	return nil
}

// PodmanServiceWait waits for a container service to be available.
func PodmanServiceWait(ctx context.Context, h Handler, id, svc string, f func(string, string) error) error {
	local, remote, err := PodmanGetAddr(ctx, h, id, svc)
	if err != nil {
		return err
	}
	return h.Backoff(ctx, func() error {
		return f(local, remote)
	})
}

// PodmanGetAddr inspects id and returns the local and remote addresses.
func PodmanGetAddr(ctx context.Context, h Handler, id, svc string) (string, string, error) {
	pod, err := ppods.Inspect(ctx, id, nil)
	if err != nil {
		return "", "", fmt.Errorf("unable to retrieve pod infra container for %s: %w", id, err)
	}
	res, err := pcontainers.Inspect(ctx, pod.InfraContainerID, nil)
	if err != nil {
		return "", "", fmt.Errorf("unable to inspect pod infra container %s: %w", ShortId(pod.InfraContainerID), err)
	}
	port, ok := res.NetworkSettings.Ports[svc]
	if !ok || len(port) == 0 {
		return "", "", fmt.Errorf("pod infra container %s does not have service %q", ShortId(pod.InfraContainerID), svc)
	}
	localIP := port[0].HostIP
	switch localIP {
	case "", "0.0.0.0", "[::]":
		localIP = "127.0.0.1"
	}
	s := svc
	if i := strings.LastIndex(s, "/"); i != -1 {
		s = s[:i]
	}
	return localIP + ":" + port[0].HostPort, "127.0.0.1:" + s, nil
}

// PodmanBuildMounts creates mount specs for a container.
func PodmanBuildMounts(mounts ...string) ([]pspec.Mount, error) {
	m := make([]pspec.Mount, len(mounts))
	for i := 0; i < len(mounts); i++ {
		j := strings.LastIndex(mounts[i], ":")
		if j == -1 {
			return nil, fmt.Errorf("mount %d is invalid %q: missing ':'", i, mounts[i])
		}
		s, err := realpath.Realpath(mounts[i][:j])
		if err != nil {
			return nil, fmt.Errorf("could not determine real path for %s: %w", mounts[i][:j], err)
		}
		m[i] = pspec.Mount{
			Type:        "bind",
			Source:      s,
			Destination: mounts[i][j+1:],
		}
	}
	return m, nil
}

// QualifiedId fully qualifies a container image id.
func QualifiedId(id string) string {
	switch strings.Count(id, "/") {
	case 0, 1:
		return "docker.io/" + id
	}
	return id
}

// ShortId truncates id to 16 characters.
func ShortId(id string) string {
	if len(id) < 16 {
		return id
	}
	return id[:16]
}

// NewPortMapping creates a port mapping from s.
func NewPortMapping(s string) (pntypes.PortMapping, error) {
	var proto string
	if i := strings.LastIndex(s, "/"); i != -1 {
		proto, s = s[i+1:], s[:i]
	}
	port, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return pntypes.PortMapping{}, err
	}
	return pntypes.PortMapping{
		ContainerPort: uint16(port),
		Protocol:      proto,
	}, nil
}
