package ionos

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/ionos-cloud/sdk-go-bundle/products/compute"
	"github.com/ionos-cloud/sdk-go-bundle/shared"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"path"
	"slices"
	"strings"
	"sync/atomic"
)

type ServerSpec struct {
	// The user data currently needs to add the ssh key to the user cause the api does not allow to add a ssh key to a private image...
	// cherry on top: would be nice if you could pass the name of the image instead of the id -- this is not possible, the name of the image is not unique
	Cores         int32   `json:"cores"`
	Image         string  `json:"image,omitempty"`
	ImagePassword string  `json:"image_password"`
	Name          string  `json:"name"`
	LanID         int32   `json:"lan_id"`
	Ram           int32   `json:"ram"`
	StorageSize   float32 `json:"storage_size"`
	TemplateID    string  `json:"template_id"`
	TemplateName  string  `json:"template_name"`
	Type          string  `json:"type"`
	UserData      string  `json:"user_data,omitempty"`
	VolumeType    string  `json:"volume_type"`
}

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

type InstanceGroup struct {
	Profile         string     `json:"profile"`
	ConfigFile      string     `json:"config_file"`
	CredentialsFile string     `json:"credentials_file"`
	Name            string     `json:"name"`
	DatacenterId    string     `json:"datacenter_id"`
	ServerSpec      ServerSpec `json:"server_spec"`

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
func BoolPtr(boolean bool) *bool { return &boolean }

// Increase implements provider.InstanceGroup.
func (i *InstanceGroup) Increase(ctx context.Context, delta int) (int, error) {
	var err error
	err = i.validateConfig()
	if err != nil {
		return 0, fmt.Errorf("validating required config: %w", err)
	}

	// Get template ID based on the provided template name.
	if i.ServerSpec.Type == "CUBE" {
		if i.ServerSpec.TemplateName != "" {
			i.ServerSpec.TemplateID, err = i.getTemplateID(i.ServerSpec.TemplateName)
			if err != nil {
				return 0, fmt.Errorf("getting template id from template name: %w", err)
			}
		}
	}

	succeeded := 0
	for range delta {
		index := int(i.instanceCounter.Add(1))
		serverData := i.getPostServerData(index)
		server, _, err2 := i.computeClient.ServersApi.DatacentersServersPost(ctx, i.DatacenterId).Server(serverData).Execute()
		if err2 != nil {
			i.log.Error("Failed to create instance", "err", err2)
			err = errors.Join(err, err2)
		} else {
			i.log.Info("Instance creation request successful", "id", *server.Id)
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
		return provider.ConnectInfo{}, fmt.Errorf("failed to get server with ID: %v, error: %w", instance, err)
	}

	var internalIP string

	nic := (*server.Entities.Nics.Items)[0]
	internalIP = (*nic.Properties.Ips)[0]

	state := *server.Metadata.State
	if state != "AVAILABLE" {
		return provider.ConnectInfo{}, fmt.Errorf("server is not in the AVAILABLE State")
	}

	connectInfo := provider.ConnectInfo{
		ConnectorConfig: i.settings.ConnectorConfig,
		ID:              *server.Id,
		InternalAddr:    internalIP,
	}

	return connectInfo, nil

}

// Update implements provider.InstanceGroup.
func (i *InstanceGroup) Update(ctx context.Context, fn func(instance string, state provider.State)) error {
	instances, _, err := i.computeClient.ServersApi.DatacentersServersGet(ctx, i.DatacenterId).Depth(2).Execute()
	if err != nil {
		return err
	}
	for _, instance := range *instances.Items {
		state := *instance.Metadata.State

		if !strings.HasPrefix(*instance.Properties.Name, "gitlab-runner-cluster") {
			continue
		}

		switch state {
		case "AVAILABLE":
			fn(*instance.Id, provider.StateRunning)
			// "BUSY" can also correspond to provider.StateDeleting but there is no way to figure
			// it out.
		case "BUSY":
			fn(*instance.Id, provider.StateCreating)
		case "INACTIVE":
			fn(*instance.Id, provider.StateDeleted)
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

// Heartbeat implements provider.InstanceGroup.
func (i *InstanceGroup) Heartbeat(ctx context.Context, instance string) error {
	_, apiResponse, err := i.computeClient.ServersApi.DatacentersServersFindById(ctx, i.DatacenterId, instance).Execute()
	if err != nil {
		if apiResponse.HttpNotFound() {
			return fmt.Errorf("instance %v does not exist", instance)
		} else {
			return fmt.Errorf("error retrieving instance %v: %w", instance, err)
		}
	}
	return nil
}

// Shutdown implements provider.InstanceGroup.
func (i *InstanceGroup) Shutdown(ctx context.Context) error {
	return nil
}

func (i *InstanceGroup) validateConfig() error {
	// Validate required attributes
	if i.ServerSpec.Type == "" || i.ServerSpec.Name == "" {
		return fmt.Errorf("type, name are required")
	}
	if i.ServerSpec.LanID == 0 || i.ServerSpec.UserData == "" || i.ServerSpec.VolumeType == "" {
		return fmt.Errorf("lan_id, user_data, volume_type are required")
	}

	// Validate type
	serverTypes := []string{"ENTERPRISE", "CUBE"}
	if !slices.Contains(serverTypes, i.ServerSpec.Type) {
		return fmt.Errorf("type can be 'ENTERPRISE' or 'CUBE'")
	}

	// Validate 'CUBE' type
	if i.ServerSpec.Type == "CUBE" {
		if i.ServerSpec.TemplateID == "" && i.ServerSpec.TemplateName == "" {
			return fmt.Errorf("one of template_id/template_name is required for 'CUBE' type, if both are specified, template_id will have priority")
		}
	}

	// Validate 'ENTERPRISE' type
	if i.ServerSpec.Type == "ENTERPRISE" {
		if i.ServerSpec.Cores == 0 || i.ServerSpec.Ram == 0 || i.ServerSpec.StorageSize == 0 {
			return fmt.Errorf("cores, ram and storage_size are required for 'ENTERPRISE' type")
		}
	}
	return nil
}

func (i *InstanceGroup) getPostServerData(index int) compute.Server {
	var serverData compute.Server
	var cores, ram *int32
	var imagePassword *string
	var storageSize *float32
	var templateID *string

	name := i.ServerSpec.Name
	serverType := i.ServerSpec.Type
	lanID := i.ServerSpec.LanID
	userdata := base64.StdEncoding.EncodeToString([]byte(i.ServerSpec.UserData))
	volumeType := i.ServerSpec.VolumeType

	if serverType == "CUBE" {
		templateID = &i.ServerSpec.TemplateID
	}

	if serverType == "ENTERPRISE" {
		cores = &i.ServerSpec.Cores
		ram = &i.ServerSpec.Ram
		storageSize = &i.ServerSpec.StorageSize
	}

	// When using public images, image password or SSH key is required at server creation, this
	// can be removed in the future if only private images will be used.
	if i.ServerSpec.ImagePassword != "" {
		imagePassword = &i.ServerSpec.ImagePassword
	}

	serverData = compute.Server{
		Entities: &compute.ServerEntities{
			Volumes: &compute.AttachedVolumes{
				Items: &[]compute.Volume{
					{
						Properties: &compute.VolumeProperties{
							Image:         &i.ServerSpec.Image,
							Type:          &volumeType,
							UserData:      &userdata,
							Size:          storageSize,
							ImagePassword: imagePassword,
						},
					},
				},
			},
			Nics: &compute.Nics{
				Items: &[]compute.Nic{
					{
						Properties: &compute.NicProperties{
							Name:           StrPtr("privateNIC"),
							Lan:            &lanID,
							FirewallActive: BoolPtr(false),
						},
					},
				},
			},
		},
		Properties: &compute.ServerProperties{
			Cores:        cores,
			Name:         StrPtr(fmt.Sprintf("%s-%d", name, index)),
			Ram:          ram,
			TemplateUuid: templateID,
			Type:         &serverType,
		},
	}
	return serverData
}

func (i *InstanceGroup) getTemplateID(templateName string) (string, error) {
	templates, _, err := i.computeClient.TemplatesApi.TemplatesGet(context.Background()).Depth(1).Execute()
	if err != nil {
		return "", err
	}
	for _, template := range *templates.Items {
		if *template.Properties.Name == templateName {
			return *template.Id, nil
		}
	}
	return "", fmt.Errorf("template %s not found", templateName)
}
