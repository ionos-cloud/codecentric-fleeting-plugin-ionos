package ionos

import (
	"context"
	"crypto"
	"encoding/base64"
	"errors"
	"fmt"
	"path"

	"golang.org/x/crypto/ssh"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/ionos-cloud/sdk-go-bundle/products/compute"
	"github.com/ionos-cloud/sdk-go-bundle/shared"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

type ExtCreateOpts struct {
	UserData string `json:"user_data,omitempty"`
	Image    string `json:"image,omitempty"`
	Template string `json:"template,omitempty"`
}

type InstanceGroup struct {
	Profile         string        `json:"profile"`
	ConfigFile      string        `json:"config_file"`
	CredentialsFile string        `json:"credentials_file"`
	Name            string        `json:"name"`
	DatacenterId    string        `json:"datacenter_id"`
	ServerSpec      ExtCreateOpts `json:"server_spec"`

	log           hclog.Logger
	computeClient compute.APIClient
	size          int

	settings provider.Settings
}

// Init implements provider.InstanceGroup.
func (i *InstanceGroup) Init(ctx context.Context, logger hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	cfg := shared.NewConfigurationFromEnv()
	computeClient := compute.NewAPIClient(cfg)

	i.computeClient = *computeClient
	i.settings = settings
	i.log = logger

	_, err := ssh.ParseRawPrivateKey(settings.Key)
	if err != nil {
		return provider.ProviderInfo{}, err
	}

	return provider.ProviderInfo{
		ID:        path.Join("ionos", i.Name),
		MaxSize:   10,
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
	key, err := ssh.ParseRawPrivateKey(i.settings.Key)
	if err != nil {
		return 0, err
	}
	privKey, ok := key.(PrivPub)

	if !ok {
		return 0, fmt.Errorf("key doesn't export PublicKey()")
	}
	sshPubKey, err := ssh.NewPublicKey(privKey.Public())

	keys := []string{
		string(ssh.MarshalAuthorizedKey(sshPubKey)),
	}
	succeeded := 0
	userdata := base64.StdEncoding.EncodeToString([]byte(i.ServerSpec.UserData))
	for range delta {

		var lanId int32

		lanId = 1
		server, _, err2 := i.computeClient.ServersApi.DatacentersServersPost(ctx, i.DatacenterId).Server(compute.Server{
			Entities: &compute.ServerEntities{
				Volumes: &compute.AttachedVolumes{
					Items: &[]compute.Volume{
						compute.Volume{
							Properties: &compute.VolumeProperties{
								Image:    &i.ServerSpec.Image,
								SshKeys:  &keys,
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
				Type:         StrPtr("CUBE"),
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
	instances, err := i.getInstances(ctx)
	if err != nil {
		return err
	}
	for _, instance := range instances {
		state := *instance.Metadata.State

		// *AVAILABLE* There are no pending modification requests for this item;
		// *BUSY* There is at least one modification request pending and all following requests will be queued;
		// *INACTIVE* Resource has been de-provisioned;

		// *DEPLOYING* Resource state DEPLOYING - relevant for Kubernetes cluster/nodepool;
		// *ACTIVE* Resource state ACTIVE - relevant for Kubernetes cluster/nodepool;
		// *FAILED* Resource state FAILED - relevant for Kubernetes cluster/nodepool;
		// *SUSPENDED* Resource state SUSPENDED - relevant for Kubernetes cluster/nodepool;
		// *FAILED_SUSPENDED* Resource state FAILED_SUSPENDED - relevant for Kubernetes cluster;
		// *UPDATING* Resource state UPDATING - relevant for Kubernetes cluster/nodepool;
		// *FAILED_UPDATING* Resource state FAILED_UPDATING - relevant for Kubernetes cluster/nodepool;
		// *DESTROYING* Resource state DESTROYING - relevant for Kubernetes cluster;
		// *FAILED_DESTROYING* Resource state FAILED_DESTROYING - relevant for Kubernetes cluster/nodepool;
		// *TERMINATED* Resource state TERMINATED - relevant for Kubernetes cluster/nodepool.
		switch state {
		case "AVAILABLE":
			fn(*instance.Id, provider.StateRunning)
		case "BUSY":
			fn(*instance.Id, provider.StateDeleting)
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

// Helper function not required by the interface
func (i *InstanceGroup) getInstances(ctx context.Context) ([]compute.Server, error) {
	servers, _, err := i.computeClient.ServersApi.DatacentersServersGet(ctx, i.DatacenterId).Depth(2).Execute()
	if err != nil {
		return nil, err
	}
	// Need filter to not return the bastion vm if the bastian vm runs in the same datacenter
	return *servers.Items, nil
}
