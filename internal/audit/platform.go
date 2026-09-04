package audit

import (
	"os"
	"strings"

	pb "github.com/sentinelgo/synergy-common/pkg/activity-logger/proto"
)

// currentPlatform maps the ENV environment variable to the activity-logger's
// Platform enum, identifying which Synergy deployment an event originated
// from. Mirrors synergy-sidecar's CurrentPlatform (internal/sidecar/model)
// exactly, including reading the env var directly rather than threading it
// through viper/Config, and matching "prod" explicitly rather than treating
// it as the fallback, so an ENV value this function doesn't recognize —
// unset, a typo, "local" — can never be silently reported as production.
// Those all fall through to Platform_NONE instead.
//
// This deliberately reads ENV, not ACTIVE_ENVIRONMENT — the latter is
// config.yml's own template token (e.g. pulsar.topic), substituted at
// deploy time by the Jenkins pipeline into the rendered config secret, and
// is never itself exported into the container's runtime environment; only
// ENV=${ACTIVE_ENVIRONMENT} is (see deploy/docker-stack.*.yml). A live
// os.Getenv("ACTIVE_ENVIRONMENT") in the running container would always see
// an unset var and silently report Platform_NONE regardless of environment.
func currentPlatform() pb.Platform {
	switch strings.ToLower(os.Getenv("ENV")) {
	case "dev":
		return pb.Platform_SYNDEV
	case "qa":
		return pb.Platform_SYNQA
	case "demo":
		return pb.Platform_SYNDEMO
	case "prod":
		return pb.Platform_SYN
	default:
		return pb.Platform_NONE
	}
}
