package cmd

import (
	"github.com/sentinelgo/synergy-geofence/internal/handler"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "start geofence service",
	Run: func(cmd *cobra.Command, args []string) {
		handler.Execute()
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
}
