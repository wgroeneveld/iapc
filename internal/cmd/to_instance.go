package cmd

import (
	"fmt"

	"github.com/wgroeneveld/iapc/iap"
	"github.com/wgroeneveld/iapc/internal/proxy"
	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"
)

var (
	zone       string
	ninterface string
)

var instanceCmd = &cobra.Command{
	Use:  "to-instance",
	Long: "Create a tunnel to a remote Compute Engine instance",
	Args: cobra.ExactArgs(1),
	PreRun: func(cmd *cobra.Command, args []string) {
		log.Info("Starting proxy", "dest", fmt.Sprintf("%v:%v", args[0], port), "port", port, "project", project)
	},
	Run: func(cmd *cobra.Command, args []string) {
		opts := []iap.DialOption{
			iap.WithProject(project),
			iap.WithInstance(args[0], zone, ninterface),
			iap.WithPort(fmt.Sprint(port)),
			iap.WithTokenSource(tokenSource()),
		}
		if compress {
			opts = append(opts, iap.WithCompression())
		}

		proxy.Start(listen, opts)
	},
}

func init() {
	instanceCmd.Flags().StringVarP(&zone, "zone", "z", "", "Target zone name")
	instanceCmd.Flags().StringVarP(&ninterface, "interface", "i", "nic0", "Target network interface")
	instanceCmd.MarkFlagRequired("zone")

	rootCmd.AddCommand(instanceCmd)
}
