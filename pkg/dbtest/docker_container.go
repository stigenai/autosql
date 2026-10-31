package dbtest

import (
	"context"
	"encoding/json"
	"strings"
)

const ownershipLabel = "dev.autosql.lease"
const dockerContainerTemplate = `{"id":{{json .Id}},"name":{{json .Name}},"image_id":{{json .Image}},"image":{{json .Config.Image}},"labels":{{json .Config.Labels}},"status":{{json .State.Status}},"running":{{json .State.Running}},"mounts":{{json .Mounts}},"ports":{{json .NetworkSettings.Ports}},"privileged":{{json .HostConfig.Privileged}},"network":{{json .HostConfig.NetworkMode}},"tmpfs":{{json .HostConfig.Tmpfs}},"binds":{{json .HostConfig.Binds}},"mount_specs":{{json (index .HostConfig "Mounts")}}}`

type dockerContainer struct {
	Tmpfs      map[string]string       `json:"tmpfs"`
	Binds      []string                `json:"binds"`
	MountSpecs []json.RawMessage       `json:"mount_specs"`
	ID         string                  `json:"id"`
	Name       string                  `json:"name"`
	ImageID    string                  `json:"image_id"`
	Image      string                  `json:"image"`
	Labels     map[string]string       `json:"labels"`
	Status     string                  `json:"status"`
	Running    bool                    `json:"running"`
	Mounts     []dockerMount           `json:"mounts"`
	Ports      map[string][]dockerPort `json:"ports"`
	Privileged bool                    `json:"privileged"`
	Network    string                  `json:"network"`
}
type dockerMount struct {
	Type        string
	Destination string
}
type dockerPort struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

func (d dockerCLI) containerIDs(ctx context.Context, filter string) ([]string, error) {
	raw, err := d.run(ctx, nil, "container", "ls", "--all", "--no-trunc", "--quiet", "--filter", filter)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(raw))
	if len(ids) > 1 {
		return nil, ErrOwnership
	}
	for _, id := range ids {
		if !leaseContainerID.MatchString(id) {
			return nil, ErrOwnership
		}
	}
	return ids, nil
}

func (d dockerCLI) locateContainer(ctx context.Context, resource leaseResource) (string, error) {
	ids, err := d.containerIDs(ctx, "name=^/"+resource.Name+"$")
	if err != nil {
		return "", err
	}
	if len(ids) == 1 {
		if resource.ID != "" && resource.ID != ids[0] {
			return "", ErrOwnership
		}
		return ids[0], nil
	}
	if resource.ID == "" {
		return "", nil
	}
	ids, err = d.containerIDs(ctx, "id="+resource.ID)
	if err != nil || len(ids) == 0 {
		return "", err
	}
	if ids[0] != resource.ID {
		return "", ErrOwnership
	}
	return ids[0], nil
}

func (d dockerCLI) ownedContainer(ctx context.Context, lease leaseRecord, resource leaseResource) (dockerContainer, bool, error) {
	id, err := d.locateContainer(ctx, resource)
	if err != nil || id == "" {
		return dockerContainer{}, false, err
	}
	raw, err := d.run(ctx, nil, "container", "inspect", "--format", dockerContainerTemplate, id)
	if err != nil {
		return dockerContainer{}, false, err
	}
	var container dockerContainer
	if json.Unmarshal(raw, &container) != nil {
		return dockerContainer{}, false, ErrOwnership
	}
	if err := verifyDockerContainer(container, lease, resource); err != nil {
		return dockerContainer{}, false, err
	}
	return container, true, nil
}

func verifyDockerContainer(c dockerContainer, lease leaseRecord, resource leaseResource) error {
	if !leaseContainerID.MatchString(c.ID) || resource.ID != "" && c.ID != resource.ID || c.Name != "/"+resource.Name {
		return ErrOwnership
	}
	if c.ImageID != resource.ImageID || c.Image != lease.Factory.Image || c.Labels[ownershipLabel] != resource.Marker {
		return ErrOwnership
	}
	if !safeContainerIsolation(c, lease.Factory.PostgresMajor) {
		return ErrOwnership
	}
	return nil
}

func safeContainerIsolation(c dockerContainer, major int) bool {
	if c.Privileged || c.Network != "bridge" || len(c.Binds) != 0 || len(c.MountSpecs) != 0 {
		return false
	}
	if len(c.Tmpfs) != 1 || c.Tmpfs[containerDataPath(major)] != containerTmpfsOptions {
		return false
	}
	return safeContainerMounts(c, major)
}
func safeContainerMounts(c dockerContainer, major int) bool {
	if len(c.Mounts) == 0 {
		return true // Docker reports --tmpfs in HostConfig.Tmpfs, not always Mounts.
	}
	return len(c.Mounts) == 1 && c.Mounts[0].Type == "tmpfs" && c.Mounts[0].Destination == containerDataPath(major)
}

func (c dockerContainer) postgresPort(expectedHost string) (string, error) {
	ports := c.Ports["5432/tcp"]
	if len(c.Ports) != 1 || len(ports) != 1 || ports[0].HostIP != expectedHost {
		return "", ErrOwnership
	}
	return ports[0].HostPort, nil
}
