package main

import (
	"github.com/codecentric/fleeting-plugin-ionos"
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"
)

func main() {
	plugin.Main(&ionos.InstanceGroup{}, ionos.Version)
}
