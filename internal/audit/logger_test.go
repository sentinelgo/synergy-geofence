package audit

import (
	"context"
	"testing"

	"github.com/sentinelgo/synergy-common/pkg/log"
)

// noopLogger's own type is unexported in synergy-common, so these tests
// can't assert "is NoOp" by type; instead they confirm the documented
// fallback behavior: construction never fails, Log never panics without a
// reachable server, and Close returns cleanly and promptly (a real,
// connected client would still satisfy this, but would need a running
// activity-log server to test against — out of scope here).
func TestNewLogger_DisabledFallsBackToNoOp(t *testing.T) {
	l := NewLogger(Config{Disabled: true, Address: "localhost:50051"}, log.Default())
	l.Log(context.Background(), nil)
	if err := l.Close(); err != nil {
		t.Errorf("Close() = %v, want nil for a no-op logger", err)
	}
}

func TestNewLogger_UnsetAddressFallsBackToNoOp(t *testing.T) {
	l := NewLogger(Config{}, log.Default())
	l.Log(context.Background(), nil)
	if err := l.Close(); err != nil {
		t.Errorf("Close() = %v, want nil for a no-op logger", err)
	}
}
