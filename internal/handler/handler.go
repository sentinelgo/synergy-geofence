package handler

import (
	"context"
	"errors"
	"fmt"
	gohttp "net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pkg "github.com/sentinelgo/synergy-geofence/internal"
	"github.com/sentinelgo/synergy-geofence/internal/audit"
	"github.com/sentinelgo/synergy-geofence/internal/database"
	"github.com/sentinelgo/synergy-geofence/internal/database/model"
	localerrors "github.com/sentinelgo/synergy-geofence/internal/errors"
	events2 "github.com/sentinelgo/synergy-geofence/internal/events"
	"github.com/sentinelgo/synergy-geofence/internal/swagger"
	"github.com/spf13/viper"
	swaggerfiles "github.com/swaggo/files"
	ginswagger "github.com/swaggo/gin-swagger"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/plugin/opentelemetry/tracing"

	activitylogger "github.com/sentinelgo/synergy-common/pkg/activity-logger"
	"github.com/sentinelgo/synergy-common/pkg/config"
	db "github.com/sentinelgo/synergy-common/pkg/database"
	"github.com/sentinelgo/synergy-common/pkg/http/token"
	"github.com/sentinelgo/synergy-common/pkg/log"
	"github.com/sentinelgo/synergy-common/pkg/otelx"
	gincommon "github.com/sentinelgo/synergy-common/pkg/otelx/gin"
)

// shutdownGracePeriod bounds the whole post-signal shutdown path: draining
// in-flight HTTP requests via srv.Shutdown, then closing the Pulsar
// publisher and the activity logger (see gracefulShutdown). Mirrors
// synergy-sidecar's own shutdownGracePeriod (see its app.go) — same class
// of problem, same budget — rather than inventing a new one. Unlike
// sidecar, geofence has no async background work analogous to its
// role-enrichment lookups, so there's no second phase competing for this
// same budget.
const shutdownGracePeriod = 15 * time.Second

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

	if cfg.Common.Jwt.HasJwtVerification() {
		if viper.GetString("common.jwt.subject") != "" {
			viper.Set("common.jwt.subject", "")
		}
	}

	// gin.Recovery() first so a panicking handler still gets a JSON 500
	// response and a logged stack trace instead of crashing the process.
	router.
		Use(gin.Recovery()).
		Use(log.CorrelationIdMiddleware())

	jwtCfg := cfg.Common.Jwt.Verification
	signingCfg := cfg.Common.Signing.Verification
	geofenceAuth, geofenceAuthErr := geofenceAuthorizationConfigFromViper()
	if geofenceAuthErr != nil {
		l.With("error", geofenceAuthErr).Error("could not initialize geofence authorization")
		panic(geofenceAuthErr)
	}
	if geofenceAuth.Enabled {
		router.Use(hydrateOpaqueGeofenceClaims(geofenceAuth, nil))
	}
	jwtEnabled := !jwtCfg.Disabled && (len(jwtCfg.Issuers) > 0 || jwtCfg.Machine.Hydra != nil)
	signingEnabled := !signingCfg.Disabled
	if jwtEnabled || signingEnabled {
		policy := token.NewPathPolicyFromConfig(jwtCfg, signingCfg, false)
		mw, terr := token.NewMiddleware(jwtCfg, signingCfg, policy)
		if terr != nil {
			l.With("error", terr).Error("could not initialize token middleware")
			panic(terr)
		}
		router.Use(mw)
		l.With(
			"jwt", jwtEnabled && len(jwtCfg.Issuers) > 0,
			"opaque", !jwtCfg.Disabled && jwtCfg.Machine.Hydra != nil,
			"signed", signingEnabled,
		).Info("authorization token verification is enabled")
	} else {
		l.Warn("authorization token verification is disabled")
	}
	router.GET("/health", healthCheck)

	// The spec is served from a path outside /swagger/*any: gin's router
	// rejects a static path (e.g. /swagger/doc.yaml) sharing a prefix with a
	// catch-all wildcard registered on the same segment.
	router.GET("/swagger-doc.yaml", func(c *gin.Context) {
		c.Data(gohttp.StatusOK, "application/yaml", swagger.OpenAPISpec)
	})
	router.GET("/swagger/*any", ginswagger.WrapHandler(swaggerfiles.Handler, ginswagger.URL("/swagger-doc.yaml")))
	l.Info("swagger UI available at /swagger/index.html")

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

	// publisher and activityLog are both closed explicitly in the shutdown
	// paths below (the signal case and gracefulShutdown), not deferred here:
	// this function's defers never run on a normal SIGTERM — router.Run
	// (its historical equivalent below) blocks forever, so Execute() itself
	// never returns for a deferred statement to fire on. A container
	// stop/redeploy just kills the process, silently dropping whatever's
	// still queued in either client.
	var publisher events2.Publisher = events2.NoopPublisher{}
	if eventsCfg, eerr := config.FromViper[events2.Config](); eerr != nil {
		l.With("error", eerr).Warn("could not load pulsar config, geofence events will not be published")
	} else if eventsCfg.Pulsar.Enabled {
		if pulsarPublisher, perr := events2.NewPulsarPublisher(eventsCfg.Pulsar); perr != nil {
			l.With("error", perr).Error("could not connect to pulsar, geofence events will not be published")
		} else {
			publisher = pulsarPublisher
		}
	}

	auditCfg, auditCfgErr := audit.ConfigFromViper()
	if auditCfgErr != nil {
		l.With("error", auditCfgErr).Warn("could not load activity-log config, geofence audit events will not be recorded")
	}
	activityLog := audit.NewLogger(auditCfg, l)

	gc := NewGeofenceController(gdbc, cfg, publisher, activityLog)

	claimsMiddleware := claimsValidationMiddleware()

	v1 := router.Group("/api/v1")
	v1.GET("/health", healthCheck)
	agencyGeofences := v1.Group("/agencies/:agency_id/geofences")
	agencyGeofences.Use(withAgencyId, claimsMiddleware)

	// Known Locations Management is admin-only: Agency Admin, System
	// Administrator or Global Administrator may create/update/delete;
	agencyGeofences.
		POST("", requireAgencyAdmin, gc.CreateGeofence).
		Handle("LIST", "", requireAgencyViewer, gc.ListGeofences).
		GET("/:geofence_id", requireAgencyViewer, gc.GetGeofence).
		PATCH("/:geofence_id", requireAgencyAdmin, gc.UpdateGeofence).
		DELETE("/:geofence_id", requireAgencyAdmin, gc.DeleteGeofence)

	srv := &gohttp.Server{
		Addr:    cfg.Common.Server.HostPort(),
		Handler: router,
	}

	serveErrCh := make(chan error, 1)
	go func() {
		if serveErr := srv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, gohttp.ErrServerClosed) {
			serveErrCh <- serveErr
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	l.With("version", fmt.Sprintf("v%s", cfg.Version)).Info("geofence service ready")

	select {
	case serveErr := <-serveErrCh:
		l.With("error", serveErr).Error("geofence service failed to serve — shutting down")
		publisher.Close()
		if cerr := activityLog.Close(); cerr != nil {
			l.With("error", cerr).Warn("activity logger did not close cleanly")
		}
		os.Exit(1)
	case <-quit:
		l.Info("shutdown signal received, draining in-flight requests")
	}

	gracefulShutdown(l, srv, publisher, activityLog)
	l.Info("geofence service ended")
}

// shutdownServer is the subset of *gohttp.Server's lifecycle gracefulShutdown
// needs, so tests can substitute a fake instead of binding a real port.
type shutdownServer interface {
	Shutdown(ctx context.Context) error
}

// gracefulShutdown drains in-flight HTTP requests via srv.Shutdown, then
// closes publisher and activityLog — in that order, so no new
// HTTP-triggered event starts publishing after the queues begin draining.
// All three share shutdownGracePeriod as a single deadline for the first
// step; publisher.Close and activityLog.Close take no context of their own
// (see events2.Publisher.Close and activitylogger.Logger.Close — the
// latter has its own independent internal timeout, 12s by default per
// synergy-common's activitylogger.WithCloseTimeout) and so aren't bounded
// by it directly, but both are expected to return quickly once the server
// has stopped accepting new requests.
func gracefulShutdown(l log.Interface, srv shutdownServer, publisher events2.Publisher, activityLog activitylogger.Logger) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		l.With("error", err).Warn("graceful shutdown did not complete cleanly")
	}

	publisher.Close()
	if err := activityLog.Close(); err != nil {
		l.With("error", err).Warn("activity logger did not close cleanly")
	}
}

func healthCheck(c *gin.Context) {
	c.JSON(gohttp.StatusOK, gin.H{
		"status":  "healthy",
		"service": fmt.Sprintf("geofence v%s branch=%s date=%s", viper.GetString("version"), viper.GetString("branch"), viper.GetString("date")),
	})
}

// withAgencyId parses the :agency_id path parameter shared by every
// geofence route into a UUID and stores it on the gin context
func withAgencyId(c *gin.Context) {
	ctx, span := otelx.StartTracer(gincommon.UnwrapContext(c))
	defer otelx.End(span, nil)
	if deferred, ok := gincommon.UpdateContext(c, ctx); ok {
		defer deferred.Defer()
	}

	agencyIdParam := c.Param("agency_id")
	if len(agencyIdParam) == 0 {
		c.AbortWithStatusJSON(gohttp.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrMissingFields, localerrors.AgencyId)})
		return
	}

	agencyId, err := uuid.Parse(agencyIdParam)
	if err != nil {
		c.AbortWithStatusJSON(gohttp.StatusBadRequest, gin.H{pkg.ErrorKey: localerrors.NewError(localerrors.ErrUUIDParse, localerrors.AgencyId)})
		return
	}

	c.Set(pkg.AgencyIDKey(), agencyId)
	c.Next()
}
