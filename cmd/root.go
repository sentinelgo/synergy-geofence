package cmd

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/sentinelgo/synergy-common/pkg/config"
)

var (
	cfgFiles                     []string
	level, version, branch, date string
)

var rootCmd = &cobra.Command{
	Use:   "synergy-geofence",
	Short: "synergy geofence service",
	Long: `synergy geofence service

CRUD + spatial lookup APIs for agency/client geofences.`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVarP(&level, "level", "l", "debug", "log level (debug, info, warn, error, debug+1, etc)")
	rootCmd.PersistentFlags().StringSliceVar(&cfgFiles, "config", []string{}, "config file(s) - multiple config files are merged with last specified file having highest priority")
}

func initConfig() {
	config.InitConfig(cfgFiles, level, version)
	if len(branch) > 0 {
		viper.Set("branch", branch)
	}
	if len(date) > 0 {
		viper.Set("date", date)
	}
}
