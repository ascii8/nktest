package nktest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
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

// PodmanOpen opens a podman context. If no client exists in the current
// context, then a new context is created and merged with the parent context,
// otherwise ctx is passed through unchanged.
func PodmanOpen(ctx context.Context) (context.Context, error) {
	// no podman client was passed with the context
	if _, err := pbindings.GetClient(ctx); err != nil {
		infos, err := BuildPodmanConnInfo(ctx)
		if err != nil {
			return nil, err
		}
		var firstErr error
		for _, info := range infos {
			var conn context.Context
			var err error
			if info.Identity == "" {
				conn, err = pbindings.NewConnection(context.Background(), info.URI)
			} else {
				conn, err = pbindings.NewConnectionWithIdentity(context.Background(), info.URI, info.Identity)
			}
			if err == nil {
				if info.Identity != "" {
					Logf(ctx, "% 16s: %s IDENTITY: %s", "PODMAN", info.URI, info.Identity)
				} else {
					Logf(ctx, "% 16s: %s", "PODMAN", info.URI)
				}
				ctx, _ := onecontext.Merge(ctx, conn)
				return context.WithValue(ctx, podmanConnKey, conn), nil
			} else if firstErr == nil {
				firstErr = err
			}
		}
		return nil, fmt.Errorf("unable to open podman connection: %w", firstErr)
	}
	return ctx, nil
}

// PodmanPullImages grabs image ids when not present on the host or when
// AlwaysPull returns true.
func PodmanPullImages(ctx context.Context, ids ...string) error {
	for _, id := range ids {
		// skip if the image exists
		if img, err := pimages.GetImage(ctx, id, nil); err == nil && !AlwaysPull(ctx) {
			Logf(ctx, "% 16s: %s %s", "EXISTING", id, ShortId(img.ID))
			continue
		}
		Logf(ctx, "% 16s: %s", "PULLING", id)
		if _, err := pimages.Pull(ctx, id, new(pimages.PullOptions).WithQuiet(true)); err != nil {
			return err
		}
	}
	return nil
}

// PodmanCreatePod creates a pod network.
func PodmanCreatePod(ctx context.Context, podName string, ids ...string) (string, error) {
	// inspect containder ids and get ports to publish
	var portMappings []pntypes.PortMapping
	for _, id := range ids {
		img, err := pimages.GetImage(ctx, id, nil)
		if err != nil {
			return "", fmt.Errorf("unable to inspect image %s: %w", id, err)
		}
		for k := range img.Config.ExposedPorts {
			port, err := ParsePortMapping(k)
			if err != nil {
				return "", fmt.Errorf("image %s has invalid service definition %q: %w", id, k, err)
			}
			port.HostPort = HostPortMap(ctx, id, k, port.ContainerPort, port.HostPort)
			portMappings = append(portMappings, port)
		}
	}
	// create spec
	var err error
	spec := pspecgen.NewPodSpecGenerator()
	spec.InfraName = podName
	if spec, err = pentities.ToPodSpecGen(*spec, &pentities.PodCreateOptions{
		Name:  podName,
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
		<-time.After(NetworkRemoveDelay(ctx))
		Logf(ctx, "% 16s: %s %s", "REMOVING POD", podName, ShortId(res.Id))
		if _, err := ppods.Remove(PodmanConn(ctx), res.Id, nil); err != nil {
			Errf(ctx, "unable to remove pod %s %s: %v", podName, ShortId(res.Id), err)
		}
	}()
	return res.Id, nil
}

// PodmanRun runs a container image id.
func PodmanRun(ctx context.Context, podId, id string, env map[string]string, mounts []string, entrypoint ...string) (string, error) {
	Logf(ctx, "% 16s: %s", "RUN", id)
	// create spec
	s := pspecgen.NewSpecGenerator(id, false)
	s.Remove = true
	s.Entrypoint = entrypoint
	s.Env = env
	s.Pod = podId
	var err error
	if s.Mounts, err = PodmanBuildMounts(mounts...); err != nil {
		return "", err
	}
	// create
	res, err := pcontainers.CreateWithSpec(ctx, s, nil)
	if err != nil {
		return "", fmt.Errorf("unable to create %s: %w", id, err)
	}
	Logf(ctx, "% 16s: %s %s", "CREATED", id, ShortId(res.ID))
	go func() {
		<-ctx.Done()
		Logf(ctx, "% 16s: %s %s", "STOPPING", id, ShortId(res.ID))
		opts := new(pcontainers.StopOptions).WithTimeout(uint(ContainerRemoveDelay(ctx).Seconds()))
		if err := pcontainers.Stop(PodmanConn(ctx), res.ID, opts); err != nil && !perrors.Contains(err, pdefine.ErrNoSuchCtr) {
			Errf(ctx, "unable to stop container %s %s: %v", id, ShortId(res.ID), err)
		}
	}()
	// run
	if err := pcontainers.Start(ctx, res.ID, nil); err != nil {
		return "", fmt.Errorf("unable to start %s %s: %w", id, ShortId(res.ID), err)
	}
	Logf(ctx, "% 16s: %s %s", "RUNNING", id, ShortId(res.ID))
	return res.ID, nil
}

// PodmanFollowLogs follows the logs for a container.
func PodmanFollowLogs(ctx context.Context, id string) error {
	go func() {
		shortId := ShortId(id)
		stdout, stderr := PrefixedWriter(Stdout(ctx), shortId+": "), PrefixedWriter(Stderr(ctx), shortId+": ")
		if err := pcontainers.Attach(ctx, id, nil, stdout, stderr, nil, &pcontainers.AttachOptions{}); err != nil {
			Errf(ctx, "unable to get logs for %s: %v", shortId, err)
		}
	}()
	return nil
}

// PodmanWait waits until a container has stopped.
func PodmanWait(ctx context.Context, id string) error {
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
func PodmanServiceWait(ctx context.Context, id, svc string, f func(string, string) error) error {
	local, remote, err := PodmanGetAddr(ctx, id, svc)
	if err != nil {
		return err
	}
	return Backoff(ctx, func() error {
		return f(local, remote)
	})
}

// PodmanGetAddr inspects id and returns the local and remote addresses.
func PodmanGetAddr(ctx context.Context, id, svc string) (string, string, error) {
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
	case 0:
		return "docker.io/library/" + id
	case 1:
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

// ParsePortMapping creates a port mapping from s.
func ParsePortMapping(s string) (pntypes.PortMapping, error) {
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

// BuildPodmanConnInfo builds a list of potential podman connection info.
func BuildPodmanConnInfo(ctx context.Context) ([]PodmanConnInfo, error) {
	var infos []PodmanConnInfo
	if uri := os.Getenv("PODMAN_HOST"); uri != "" {
		infos = append(infos, PodmanConnInfo{URI: uri})
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		infos = append(infos, PodmanConnInfo{URI: "unix:" + dir + "/podman/podman.sock"})
	}
	if v, err := PodmanSystemConnectionList(ctx); err == nil && len(v) != 0 {
		infos = append(infos, v...)
	}
	infos = append(infos, PodmanConnInfo{URI: "/var/run/podman/podman.sock"})
	if uri := os.Getenv("DOCKER_HOST"); uri != "" {
		infos = append(infos, PodmanConnInfo{URI: uri})
	}
	return infos, nil
}

// PodmanSystemConnectionList executes podman system connection list to
// retrieve the remote socket list.
func PodmanSystemConnectionList(ctx context.Context) ([]PodmanConnInfo, error) {
	cmdPath, err := exec.LookPath("podman")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, cmdPath, "system", "connection", "list", "--format=json")
	buf, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	var infos []PodmanConnInfo
	if err := dec.Decode(&infos); err != nil {
		return nil, err
	}
	sort.SliceStable(infos, func(i, j int) bool {
		switch {
		case infos[i].Default:
			return true
		case infos[j].Default:
			return false
		}
		return i < j
	})
	return infos, nil
}

// PodmanConnInfo holds information about a podman connection.
type PodmanConnInfo struct {
	Name     string `json:"Name"`
	URI      string `json:"URI"`
	Identity string `json:"Identity"`
	Default  bool   `json:"Default"`
}
