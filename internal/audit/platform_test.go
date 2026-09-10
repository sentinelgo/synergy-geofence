package audit

import (
	"testing"

	pb "github.com/sentinelgo/synergy-common/pkg/activity-logger/proto"
)

func TestCurrentPlatform(t *testing.T) {
	tests := []struct {
		activeEnvironment string
		want              pb.Platform
	}{
		{activeEnvironment: "", want: pb.Platform_NONE},
		{activeEnvironment: "dev", want: pb.Platform_SYNDEV},
		{activeEnvironment: "DEV", want: pb.Platform_SYNDEV},
		{activeEnvironment: "qa", want: pb.Platform_SYNQA},
		{activeEnvironment: "demo", want: pb.Platform_SYNDEMO},
		{activeEnvironment: "prod", want: pb.Platform_SYN},
		{activeEnvironment: "PROD", want: pb.Platform_SYN},
		{activeEnvironment: "local", want: pb.Platform_NONE},
		{activeEnvironment: "some-typo", want: pb.Platform_NONE},
	}

	for _, tt := range tests {
		t.Run(tt.activeEnvironment, func(t *testing.T) {
			t.Setenv("ENV", tt.activeEnvironment)
			if got := currentPlatform(); got != tt.want {
				t.Fatalf("currentPlatform() with ENV=%q = %v, want %v", tt.activeEnvironment, got, tt.want)
			}
		})
	}
}
