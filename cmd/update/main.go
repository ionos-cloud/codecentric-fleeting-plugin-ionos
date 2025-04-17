package main

import (
	"context"
	"os"

	"github.com/codecentric/fleeting-plugin-ionos.git"
	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

func main() {
	ionos := ionos.InstanceGroup{}
	_, err := ionos.Init(context.Background(), hclog.Default(), provider.Settings{})
	if err != nil {
		println(err.Error())
		os.Exit(1)
	}
	err = ionos.Update(context.Background(), func(instance string, state provider.State) {
		println(instance, state)

	})
	if err != nil {
		println(err.Error())
		os.Exit(1)
	}

}
