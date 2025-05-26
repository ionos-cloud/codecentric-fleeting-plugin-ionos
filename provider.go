package ionos

import (
	"context"
	"crypto"
	"encoding/base64"
	"errors"
	"fmt"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/ionos-cloud/sdk-go-bundle/products/compute"
	"github.com/ionos-cloud/sdk-go-bundle/shared"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/crypto/ssh"
	"path"
	"slices"
	"strings"
	"sync/atomic"
)

type ServerSpec struct {
	// The user data currently needs to add the ssh key to the user cause the api does not allow to add a ssh key to a private image...
	// cherry on top: would be nice if you could pass the name of the image instead of the id -- this is not possible, the name of the image is not unique
	// cherry on top: would be nice if you could pass the name of the template instead of the id
	Cores        int32   `json:"cores"`
	Image        string  `json:"image,omitempty"`
	Name         string  `json:"name"`
	PrivateLANID int32   `json:"private_lan_id"`
	PublicLANID  int32   `json:"public_lan_id"`
	Ram          int32   `json:"ram"`
	StorageSize  float32 `json:"storage_size"`
	TemplateID   string  `json:"template_id"`
	TemplateName string  `json:"template_name"`
	Type         string  `json:"type"`
	UserData     string  `json:"user_data,omitempty"`
	VolumeType   string  `json:"volume_type"`
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

type PrivPub interface {
	crypto.PrivateKey
	Public() crypto.PublicKey
}

// Increase implements provider.InstanceGroup.
func (i *InstanceGroup) Increase(ctx context.Context, delta int) (int, error) {
	var err error
	err = i.validateConfig()
	if err != nil {
		return 0, fmt.Errorf("validating required config: %w", err)
	}

	succeeded := 0
	for range delta {
		index := int(i.instanceCounter.Add(1))
		var serverData compute.Server

		// TODO: The error check needs to be removed after SSH logic from readServerData is removed
		serverData, err = i.readServerData(index)
		if err != nil {
			return succeeded, fmt.Errorf("reading server data: %w", err)
		}

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
		return provider.ConnectInfo{}, fmt.Errorf("Failed to get server: %w", err)
	}

	var internalIP string
	var externalIP string

	for _, nic := range *server.Entities.Nics.Items {
		if len(*nic.Properties.Ips) > 0 {
			if strings.HasPrefix(*nic.Properties.Name, "public") {
				externalIP = (*nic.Properties.Ips)[0]
			} else if strings.HasPrefix(*nic.Properties.Name, "private") {
				internalIP = (*nic.Properties.Ips)[0]
			}
		}
	}
	if internalIP == "" && externalIP == "" {
		return provider.ConnectInfo{}, fmt.Errorf("Could not find IPs")
	}

	state := *server.Metadata.State
	if state != "AVAILABLE" {
		return provider.ConnectInfo{}, fmt.Errorf("Server is not in the AVAILABLE State")
	}

	connectInfo := provider.ConnectInfo{
		ConnectorConfig: i.settings.ConnectorConfig,
		ID:              *server.Id,
		ExternalAddr:    externalIP,
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

// Shutdown implements provider.InstanceGroup.
func (i *InstanceGroup) Shutdown(ctx context.Context) error {
	return nil
}

func (i *InstanceGroup) validateConfig() error {
	// Validate required attributes
	if i.ServerSpec.Type == "" || i.ServerSpec.Name == "" {
		return fmt.Errorf("type, name are required")
	}
	if i.ServerSpec.PublicLANID == 0 || i.ServerSpec.PrivateLANID == 0 || i.ServerSpec.UserData == "" || i.ServerSpec.VolumeType == "" {
		return fmt.Errorf("public_lan_id, private_lan_id, user_data, volume_type are required")
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

func (i *InstanceGroup) readServerData(index int) (compute.Server, error) {
	var serverData compute.Server
	// err is used for SSH and err2 for other validations, err will be removed when SSH logic will
	// be removed.
	var cores, ram *int32
	var err2 error
	var storageSize *float32
	var templateID *string

	connectInfo := provider.ConnectInfo{ConnectorConfig: i.settings.ConnectorConfig}
	name := i.ServerSpec.Name
	privateLANID := i.ServerSpec.PrivateLANID
	publicLANID := i.ServerSpec.PublicLANID
	serverType := i.ServerSpec.Type
	userdata := base64.StdEncoding.EncodeToString([]byte(i.ServerSpec.UserData))
	volumeType := i.ServerSpec.VolumeType

	if serverType == "CUBE" {
		if i.ServerSpec.TemplateID != "" {
			templateID = &i.ServerSpec.TemplateID
		} else {
			templateID, err2 = i.getTemplateID(i.ServerSpec.TemplateName)
			if err2 != nil {
				return serverData, err2
			}
		}
	}

	if serverType == "ENTERPRISE" {
		cores = &i.ServerSpec.Cores
		ram = &i.ServerSpec.Ram
		storageSize = &i.ServerSpec.StorageSize
	}

	// TODO -- The SSH logic should not be inside this method but it will be deleted anyway.
	if connectInfo.Key == nil {
		return serverData, fmt.Errorf("no private key")
	}
	var key PrivPub
	// Private key logic
	privateKey, err := ssh.ParseRawPrivateKey(connectInfo.Key)
	if err != nil {
		return serverData, fmt.Errorf("reading private key: %w", err)
	}
	var ok bool
	key, ok = privateKey.(PrivPub)
	if !ok {
		return serverData, fmt.Errorf("key doesn't export PublicKey()")
	}
	// Public key logic
	publicKey, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		return serverData, fmt.Errorf("generating ssh public key: %w", err)
	}
	publicKeyStr := string(ssh.MarshalAuthorizedKey(publicKey))
	sshKeys := []string{publicKeyStr}

	serverData = compute.Server{
		Entities: &compute.ServerEntities{
			Volumes: &compute.AttachedVolumes{
				Items: &[]compute.Volume{
					{
						Properties: &compute.VolumeProperties{
							Image:    &i.ServerSpec.Image,
							Type:     &volumeType,
							UserData: &userdata,
							Size:     storageSize,
							SshKeys:  &sshKeys,
						},
					},
				},
			},
			Nics: &compute.Nics{
				Items: &[]compute.Nic{
					{
						Properties: &compute.NicProperties{
							Name:           StrPtr("publicNIC"),
							Lan:            &publicLANID,
							FirewallActive: BoolPtr(false),
						},
					},
					{
						Properties: &compute.NicProperties{
							Name:           StrPtr("privateNIC"),
							Lan:            &privateLANID,
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
	return serverData, nil
}

// TODO -- Call this function at the beginning of 'Increase' method, not in the foor loop
func (i *InstanceGroup) getTemplateID(templateName string) (*string, error) {
	templates, _, err := i.computeClient.TemplatesApi.TemplatesGet(context.Background()).Depth(1).Execute()
	if err != nil {
		return nil, err
	}
	for _, template := range *templates.Items {
		if *template.Properties.Name == templateName {
			return template.Id, nil
		}
	}
	return nil, fmt.Errorf("template %s not found", templateName)
}
