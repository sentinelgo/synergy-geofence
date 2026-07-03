package handler

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pkg "github.com/sentinelgo/synergy-geofence/internal"
	"github.com/sentinelgo/synergy-geofence/internal/database"
	"github.com/sentinelgo/synergy-geofence/internal/database/model"
	localerrors "github.com/sentinelgo/synergy-geofence/internal/errors"
	events2 "github.com/sentinelgo/synergy-geofence/internal/events"
	"github.com/spf13/viper"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/plugin/opentelemetry/tracing"

	"github.com/sentinelgo/synergy-common/pkg/config"
	db "github.com/sentinelgo/synergy-common/pkg/database"
	"github.com/sentinelgo/synergy-common/pkg/database/migration"
	"github.com/sentinelgo/synergy-common/pkg/log"
	"github.com/sentinelgo/synergy-common/pkg/otelx"
	gincommon "github.com/sentinelgo/synergy-common/pkg/otelx/gin"

	_ "github.com/sentinelgo/synergy-geofence/internal/db"
)

func Execute() {
	log.SetDefault(log.WithLevel(log.LevelTrace))
	l := log.Default()

	gin.DefaultWriter = log.Writer(l)
	router := gin.New()
	_ = router.SetTrustedProxies(nil)

	cfg, err := config.FromViper[config.Config]()
	if err != nil {
		l.With("error", err).Error("could not load config")
		panic(err)
	}

	if cfg.Common.Tracing != nil {
		if _, err = otelx.New(cfg.Common.Tracing.ServiceName, l, cfg.Common.Tracing); err != nil {
			l.With("error", err).Warn("could not initialize tracer")
			otel.SetTracerProvider(noop.NewTracerProvider())
		} else {
			router.Use(otelgin.Middleware(cfg.Common.Tracing.ServiceName))
		}
	}

	// gin.Recovery() first so a panicking handler still gets a JSON 500
	// response and a logged stack trace instead of crashing the process.
	router.
		Use(gin.Recovery()).
		Use(log.CorrelationIdMiddleware())

	router.GET("/health", healthCheck)

	models := []interface{}{&model.Geofence{}}
	internalAdapter, err := db.NewAdapter[uuid.UUID](context.Background(), "", models...)
	if err != nil {
		l.With("error", err).Error("could not initialize database adapter")
		panic(err)
	}
	dbc, ok := db.ToUnsafeCacheDbAdapter[uuid.UUID](internalAdapter)
	if !ok || dbc == nil {
		l.Error("could not connect to database")
		panic("could not connect to database")
	}

	gdbc := database.NewGeofenceAdapter(internalAdapter)

	dbc.DB().Config.Logger = log.AsGormLogger(log.Default(), gormlogger.Silent)
	_ = dbc.DB().Use(tracing.NewPlugin(tracing.WithoutMetrics(), tracing.WithTracerProvider(otel.GetTracerProvider())))

	if err = migration.Migrate(dbc.DB()); err != nil {
		l.With("error", err).Error("could not validate database")
		panic(err)
	}

	var publisher events2.Publisher = events2.NoopPublisher{}
	if eventsCfg, eerr := config.FromViper[events2.Config](); eerr != nil {
		l.With("error", eerr).Warn("could not load pulsar config, geofence events will not be published")
	} else if eventsCfg.Pulsar.Enabled {
		if pulsarPublisher, perr := events2.NewPulsarPublisher(eventsCfg.Pulsar); perr != nil {
			l.With("error", perr).Error("could not connect to pulsar, geofence events will not be published")
		} else {
			publisher = pulsarPublisher
			defer pulsarPublisher.Close()
		}
	}

	gc := NewGeofenceController(gdbc, cfg, publisher)

	v1 := router.Group("/api/v1")
	agencyGeofences := v1.Group("/agencies/:agency_id/geofences")
	agencyGeofences.Use(withAgencyId)

	agencyGeofences.
		POST("", gc.CreateGeofence).
		GET("", gc.ListGeofences).
		GET("/lookup", gc.LookupGeofences).
		GET("/:geofence_id", gc.GetGeofence).
		PUT("/:geofence_id", gc.UpdateGeofence).
		DELETE("/:geofence_id", gc.DeleteGeofence)

	l.With("version", fmt.Sprintf("v%s", cfg.Version)).Info("geofence service ready")
	l.With("error", router.Run(cfg.Common.Server.HostPort())).Error("geofence service ended")
}

func healthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "healthy",
		"service": fmt.Sprintf("geofence v%s branch=%s date=%s", viper.GetString("version"), viper.GetString("branch"), viper.GetString("date")),
	})
}

// withAgencyId parses the :agency_id path parameter shared by every
// geofence route into a UUID and stores it on the gin context, mirroring
// sso-agency-service's own withAgencyId middleware.
func withAgencyId(c *gin.Context) {
	ctx, span := otelx.StartTracer(gincommon.UnwrapContext(c))
	defer otelx.End(span, nil)
	if deferred, ok := gincommon.UpdateContext(c, ctx); ok {
		defer deferred.Defer()
	}

	agencyIdParam := c.Param("agency_id")
	if len(agencyIdParam) == 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrMissingFields, localerrors.AgencyId)})
		return
	}

	agencyId, err := uuid.Parse(agencyIdParam)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrUUIDParse, localerrors.AgencyId)})
		return
	}

	c.Set(pkg.AgencyIDKey(), agencyId)
	c.Next()
}
