package ionos

import (
	"context"
	"crypto"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync/atomic"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/ionos-cloud/sdk-go-bundle/products/compute"
	"github.com/ionos-cloud/sdk-go-bundle/shared"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

type ExtCreateOpts struct {
	// The user data currently needs to add the ssh key to the user cause the api does not allow to add a ssh key to a private image...
	UserData string `json:"user_data,omitempty"`
	// cherry on top: would be nice if you could pass the name of the image instead of the id
	Image string `json:"image,omitempty"`
	// cherry on top: would be nice if you could pass the name of the template instead of the id
	Template string `json:"template,omitempty"`
}

type InstanceGroup struct {
	Profile         string        `json:"profile"`
	ConfigFile      string        `json:"config_file"`
	CredentialsFile string        `json:"credentials_file"`
	Name            string        `json:"name"`
	DatacenterId    string        `json:"datacenter_id"`
	ServerSpec      ExtCreateOpts `json:"server_spec"`

	log             hclog.Logger
	computeClient   compute.APIClient
	instanceCounter atomic.Int32

	settings provider.Settings
}

// Init implements provider.InstanceGroup.
func (i *InstanceGroup) Init(ctx context.Context, logger hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	cfg := shared.NewConfigurationFromEnv()
	computeClient := compute.NewAPIClient(cfg)

	i.computeClient = *computeClient
	i.settings = settings
	i.log = logger

	return provider.ProviderInfo{
		ID:        path.Join("ionos", i.Name),
		MaxSize:   1000,
		Version:   Version.String(),
		BuildInfo: Version.BuildInfo(),
	}, nil
}

func StrPtr(str string) *string {
	return &str
}
func FloatPtr(float float32) *float32 {
	return &float
}

type PrivPub interface {
	crypto.PrivateKey
	Public() crypto.PublicKey
}

// Increase implements provider.InstanceGroup.
func (i *InstanceGroup) Increase(ctx context.Context, delta int) (int, error) {
	succeeded := 0
	userdata := base64.StdEncoding.EncodeToString([]byte(i.ServerSpec.UserData))
	var err error
	for range delta {

		// TODO: Make Lan ID configurable
		var lanId int32
		lanId = 1
		index := int(i.instanceCounter.Add(1))

		server, _, err2 := i.computeClient.ServersApi.DatacentersServersPost(ctx, i.DatacenterId).Server(compute.Server{
			// TODO: Make server configurable
			Entities: &compute.ServerEntities{
				Volumes: &compute.AttachedVolumes{
					Items: &[]compute.Volume{
						{
							Properties: &compute.VolumeProperties{
								Image:    &i.ServerSpec.Image,
								Type:     StrPtr("DAS"),
								UserData: &userdata,
							},
						},
					},
				},
				Nics: &compute.Nics{
					Items: &[]compute.Nic{
						{
							Properties: &compute.NicProperties{
								Name: StrPtr("default"),
								Lan:  &lanId,
							},
						},
					},
				},
			},
			Properties: &compute.ServerProperties{
				// TODO: Make type, and name configurable
				Type:         StrPtr("CUBE"),
				Name:         StrPtr(fmt.Sprintf("gitlab-runner-cluster-%d", index)),
				TemplateUuid: &i.ServerSpec.Template,
			},
		}).Execute()

		if err2 != nil {
			i.log.Error("Failed to create instance", "err", err)
			err = errors.Join(err, err2)
		} else {
			i.log.Info("Instance creation request successful", "id", (*server.Id))
			succeeded++
		}
	}

	i.log.Info("Increase", "delta", delta, "succeeded", succeeded)

	return succeeded, err
}

// ConnectInfo implements provider.InstanceGroup.
func (i *InstanceGroup) ConnectInfo(ctx context.Context, instance string) (provider.ConnectInfo, error) {
	server, _, err := i.computeClient.ServersApi.DatacentersServersFindById(ctx, i.DatacenterId, instance).Pretty(true).Depth(2).Execute()
	if err != nil {
		return provider.ConnectInfo{}, fmt.Errorf("Failed to get server: %w", err)
	}

	var ip string
	// TODO: find internal and external ip, not the first ip for both....
	for _, nic := range *server.Entities.Nics.Items {
		if len(*nic.Properties.Ips) > 0 {
			ip = (*nic.Properties.Ips)[0]
		}
	}
	if ip == "" {
		return provider.ConnectInfo{}, fmt.Errorf("Could not find ip")
	}

	state := *server.Metadata.State
	if state != "AVAILABLE" {
		return provider.ConnectInfo{}, fmt.Errorf("Server is not in the AVAILABLE State")
	}

	return provider.ConnectInfo{
		ConnectorConfig: i.settings.ConnectorConfig,
		ID:              *server.Id,
		ExternalAddr:    ip,
		InternalAddr:    ip,
	}, nil

}

// Update implements provider.InstanceGroup.
func (i *InstanceGroup) Update(ctx context.Context, fn func(instance string, state provider.State)) error {
	instances, _, err := i.computeClient.ServersApi.DatacentersServersGet(ctx, i.DatacenterId).Depth(2).Execute()
	if err != nil {
		return err
	}
	for _, instance := range *instances.Items {
		state := *instance.Metadata.State

		// TODO: It would be great if we had a better way to identify which
		// server belong to the gitlab runner...
		// something like labels on the servers would be nice
		if !strings.HasPrefix(*instance.Properties.Name, "gitlab-runner-cluster") {
			continue
		}

		switch state {
		case "AVAILABLE":
			fn(*instance.Id, provider.StateRunning)
		// TODO: is there a better way to know if a server is created or destroyed
		// These are the Gitlab Plugin states for servers...
		// StateCreating State = "creating"
		// StateRunning  State = "running"
		// StateDeleting State = "deleting"
		// StateDeleted  State = "deleted"
		// StateTimeout  State = "timeout"
		case "BUSY":
			fn(*instance.Id, provider.StateCreating)
		case "INACTIVE":
			fn(*instance.Id, provider.StateDeleting)

		}

	}
	return nil
}

// Decrease implements provider.InstanceGroup.
func (i *InstanceGroup) Decrease(ctx context.Context, instances []string) ([]string, error) {
	if len(instances) == 0 {
		return nil, nil
	}

	succeeded := make([]string, 0, len(instances))
	var err error
	for _, id := range instances {
		_, err2 := i.computeClient.ServersApi.DatacentersServersDelete(ctx, i.DatacenterId, id).Execute()
		if err2 != nil {
			i.log.Error("Failed to delete instance", "err", err2, "id", id)
			err = errors.Join(err, err2)
		} else {
			i.log.Info("Instance deletion request successful", "id", id)
			succeeded = append(succeeded, id)
		}
	}

	i.log.Info("Decrease", "instances", instances)

	return succeeded, err
}

// Shutdown implements provider.InstanceGroup.
func (i *InstanceGroup) Shutdown(ctx context.Context) error {
	return nil
}
