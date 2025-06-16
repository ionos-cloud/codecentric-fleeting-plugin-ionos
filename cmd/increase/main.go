package main

import (
	"context"
	"os"

	"github.com/hashicorp/go-hclog"

	"github.com/codecentric/fleeting-plugin-ionos"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

func main() {
	ionos := ionos.InstanceGroup{}
	_, err := ionos.Init(context.Background(), hclog.Default(), provider.Settings{})
	if err != nil {
		println(err.Error())
		os.Exit(1)
	}
	_, err = ionos.Increase(context.Background(), 1)
	if err != nil {
		println(err.Error())
		os.Exit(1)
	}

}
