package db

import (
	"gorm.io/gorm"

	"github.com/sentinelgo/synergy-common/pkg/database/migration"

	"github.com/sentinelgo/synergy-geofence/internal/db/initial/oracle"
)

func init() {
	migration.RegisterInitialMigration(&migration.InitialMigration{
		InitSchema: func(tx *gorm.DB) error {
			if err := tx.Migrator().AutoMigrate(&oracle.Geofence{}); err != nil {
				return err
			}
			return oracle.CreateSpatialIndex(tx)
		},
	})
}
