package cmd

import (
	"context"

	"github.com/google/uuid"
	"github.com/sentinelgo/synergy-geofence/internal/database/model"
	"github.com/spf13/cobra"

	"github.com/sentinelgo/synergy-common/pkg/config"
	"github.com/sentinelgo/synergy-common/pkg/database"
	"github.com/sentinelgo/synergy-common/pkg/database/migration"
	"github.com/sentinelgo/synergy-common/pkg/log"

	_ "github.com/sentinelgo/synergy-geofence/internal/db"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "migrate the database",
	Run: func(cmd *cobra.Command, args []string) {
		l := log.Default()

		cfg, err := config.FromViper[config.Config]()
		if err != nil {
			l.With("error", err).Error("config load failed")
			return
		}

		adapter, err := database.NewAdapter[uuid.UUID](context.Background(), "", &model.Geofence{})
		if err != nil {
			l.With("error", err).Error("could not initialize database adapter")
			return
		}

		dbc, ok := database.ToUnsafeCacheDbAdapter[uuid.UUID](adapter)
		if !ok || dbc == nil {
			l.Error("could not connect to database")
			return
		}

		if err = migration.Migrate(dbc.DB()); err != nil {
			l.With("error", err).Error("migration failed")
			return
		}

		l.With("version", cfg.Version).Info("migration ran successfully")
	},
}

func init() {
	rootCmd.AddCommand(migrateCmd)
}
