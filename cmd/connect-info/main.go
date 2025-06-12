package main

import (
	"bufio"
	"context"
	"fmt"
	"os"

	"github.com/codecentric/fleeting-plugin-ionos"
	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

func main() {

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Enter uuid of server to connect: ")
	input, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("Error reading input:", err)
		return
	}

	// The input string will include the newline character at the end.
	// You might want to trim it.
	input = input[:len(input)-1] // Remove the trailing newline
	ionos := ionos.InstanceGroup{}
	_, err = ionos.Init(context.Background(), hclog.Default(), provider.Settings{})
	if err != nil {
		println(err.Error())
		os.Exit(1)
	}
	info, err := ionos.ConnectInfo(context.Background(), input)
	if err != nil {
		println(err.Error())
		os.Exit(1)
	}
	fmt.Printf("info: %+v\n", info)
}
