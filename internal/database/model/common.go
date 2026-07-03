package model

import (
	"time"

	"github.com/google/uuid"
	"github.com/sentinelgo/synergy-common/pkg/database/model"
)

type MutableModel model.MutableModel[uuid.UUID]

func DefaultMutableModel() MutableModel {
	return MutableModel{ID: uuid.New(), Version: 1}
}

func NewMutableModel(id uuid.UUID, now time.Time) MutableModel {
	return MutableModel{ID: id, CreatedAt: now.Truncate(time.Microsecond), Version: 1}
}
