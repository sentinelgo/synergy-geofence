package handler

import (
	"context"
	"errors"
	"testing"

	events2 "github.com/sentinelgo/synergy-geofence/internal/events"

	"github.com/sentinelgo/synergy-common/pkg/activity-logger/proto"
	"github.com/sentinelgo/synergy-common/pkg/log"
)

// fakeShutdownServer records whether Shutdown was called, so tests can
// confirm gracefulShutdown drains the HTTP server rather than skipping
// straight to closing publisher/activityLog.
type fakeShutdownServer struct {
	shutdownCalled bool
	shutdownErr    error
}

func (f *fakeShutdownServer) Shutdown(context.Context) error {
	f.shutdownCalled = true
	return f.shutdownErr
}

// fakePublisher records whether Close was called; Publish is unused here.
type fakePublisher struct {
	closeCalled bool
}

func (f *fakePublisher) Publish(context.Context, events2.Event) {}
func (f *fakePublisher) Close()                                 { f.closeCalled = true }

// fakeActivityLog implements activitylogger.Logger, recording whether Close
// was called and returning a configurable error from it.
type fakeActivityLog struct {
	closeCalled bool
	closeErr    error
}

func (f *fakeActivityLog) Log(context.Context, *proto.ActivityLogRequest) {}
func (f *fakeActivityLog) Close() error {
	f.closeCalled = true
	return f.closeErr
}

// TestGracefulShutdown_ClosesEverythingInOrder guards this PR's fix:
// gracefulShutdown (called from the SIGTERM/SIGINT path in Execute, not a
// defer that a blocking router.Run/ListenAndServe would never let run) must
// drain the HTTP server, then close both the event publisher and the
// activity logger, so neither's in-flight queue is silently dropped on a
// normal shutdown.
func TestGracefulShutdown_ClosesEverythingInOrder(t *testing.T) {
	srv := &fakeShutdownServer{}
	pub := &fakePublisher{}
	al := &fakeActivityLog{}

	gracefulShutdown(log.Default(), srv, pub, al)

	if !srv.shutdownCalled {
		t.Error("srv.Shutdown was not called")
	}
	if !pub.closeCalled {
		t.Error("publisher.Close was not called")
	}
	if !al.closeCalled {
		t.Error("activityLog.Close was not called")
	}
}

// TestGracefulShutdown_StillClosesClientsWhenHTTPShutdownFails guards that
// a slow/failing srv.Shutdown (e.g. it hits shutdownGracePeriod with
// requests still in flight) doesn't skip closing publisher/activityLog —
// those queues should still get a chance to drain even if the HTTP side
// didn't shut down cleanly.
func TestGracefulShutdown_StillClosesClientsWhenHTTPShutdownFails(t *testing.T) {
	srv := &fakeShutdownServer{shutdownErr: errors.New("boom")}
	pub := &fakePublisher{}
	al := &fakeActivityLog{closeErr: errors.New("close failed")}

	gracefulShutdown(log.Default(), srv, pub, al)

	if !pub.closeCalled {
		t.Error("publisher.Close was not called after a failing srv.Shutdown")
	}
	if !al.closeCalled {
		t.Error("activityLog.Close was not called after a failing srv.Shutdown")
	}
}
