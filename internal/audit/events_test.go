package audit

import (
	"context"
	"testing"

	"github.com/google/uuid"

	activitylogger "github.com/sentinelgo/synergy-common/pkg/activity-logger"
	pb "github.com/sentinelgo/synergy-common/pkg/activity-logger/proto"
)

// captureLogger is an activitylogger.Logger fake that records every request
// it was handed, so tests can assert on the exact request(s)
// LogGeofenceCreated/Updated/Deleted build — LogGeofenceUpdated in
// particular can emit more than one per call.
type captureLogger struct {
	reqs []*pb.ActivityLogRequest
}

func (c *captureLogger) Log(_ context.Context, req *pb.ActivityLogRequest) {
	c.reqs = append(c.reqs, req)
}
func (c *captureLogger) Close() error { return nil }

// action returns the single captured request with the given Action, or
// nil if none (or more than one — a test bug) matches.
func (c *captureLogger) action(action string) *pb.ActivityLogRequest {
	var found *pb.ActivityLogRequest
	for _, r := range c.reqs {
		if r.GetAction() == action {
			if found != nil {
				return nil // ambiguous — caller should treat as "not found"
			}
			found = r
		}
	}
	return found
}

var _ activitylogger.Logger = (*captureLogger)(nil)

func TestLogGeofenceCreated(t *testing.T) {
	l := &captureLogger{}
	userID := uuid.New()
	geofenceID := uuid.New()
	agencyID := uuid.New()

	LogGeofenceCreated(context.Background(), l,
		Actor{UserID: userID, IP: "10.0.0.1"},
		Resource{ID: geofenceID, Name: "Downtown Office", AgencyID: agencyID},
		map[string]any{"name": "Downtown Office"},
	)

	if len(l.reqs) != 1 {
		t.Fatalf("Log was called %d times, want 1", len(l.reqs))
	}
	req := l.reqs[0]

	if req.GetAction() != actionCreated {
		t.Errorf("Action = %q, want %q", req.GetAction(), actionCreated)
	}
	if req.GetType() != pb.EventType_ACTIVITY {
		t.Errorf("Type = %v, want ACTIVITY", req.GetType())
	}
	if req.GetCategory() != pb.EventCategory_CREATED {
		t.Errorf("Category = %v, want CREATED", req.GetCategory())
	}
	if req.GetStatus() != pb.ActionStatus_SUCCESS {
		t.Errorf("Status = %v, want SUCCESS", req.GetStatus())
	}
	if req.GetMessage() != messageCreated {
		t.Errorf("Message = %q, want %q", req.GetMessage(), messageCreated)
	}

	// Resource.Type stays RESOURCE_OTHER — synergy-common still has no
	// ResourceType_GEOFENCE (see this package's doc comment in events.go).
	if got := req.GetResource().GetType(); got != pb.ResourceType_RESOURCE_OTHER {
		t.Errorf("Resource.Type = %v, want RESOURCE_OTHER (placeholder)", got)
	}
	if req.GetResource().GetId() != geofenceID.String() {
		t.Errorf("Resource.Id = %q, want %q", req.GetResource().GetId(), geofenceID.String())
	}
	if req.GetResource().GetDisplayName() != "Downtown Office" {
		t.Errorf("Resource.DisplayName = %q, want %q", req.GetResource().GetDisplayName(), "Downtown Office")
	}
	if req.GetResource().GetParentType() != pb.ResourceType_AGENCY || req.GetResource().GetParentId() != agencyID.String() {
		t.Errorf("Resource parent = (%v, %q), want (AGENCY, %q)", req.GetResource().GetParentType(), req.GetResource().GetParentId(), agencyID.String())
	}
	if req.Metadata["resource_kind"] != resourceKindGeofence {
		t.Errorf(`Metadata["resource_kind"] = %q, want %q`, req.Metadata["resource_kind"], resourceKindGeofence)
	}

	if req.GetActor().GetId() != userID.String() {
		t.Errorf("Actor.Id = %q, want %q", req.GetActor().GetId(), userID.String())
	}
	if req.GetActor().GetAgencyId() != agencyID.String() {
		t.Errorf("Actor.AgencyId = %q, want %q", req.GetActor().GetAgencyId(), agencyID.String())
	}
	if req.GetActor().GetIp() != "10.0.0.1" {
		t.Errorf("Actor.Ip = %q, want %q", req.GetActor().GetIp(), "10.0.0.1")
	}
	if req.GetDetails() == "" {
		t.Error("Details was not set from the non-nil details argument")
	}
}

func TestLogGeofenceUpdated(t *testing.T) {
	tests := []struct {
		name        string
		diff        map[string]any
		wantActions []string
	}{
		{
			name:        "generic detail field only",
			diff:        map[string]any{"name": "New Name"},
			wantActions: []string{actionUpdated},
		},
		{
			name:        "geometry only",
			diff:        map[string]any{"type": "polygon", "geo_json": "{}"},
			wantActions: []string{actionGeometryUpdated},
		},
		{
			name:        "status only",
			diff:        map[string]any{"status": "inactive"},
			wantActions: []string{actionStatusChanged},
		},
		{
			name:        "colocation exclusion only",
			diff:        map[string]any{"exclude_from_colocation": true},
			wantActions: []string{actionColocationExclusionChanged},
		},
		{
			name:        "geometry and status together emit two distinct events",
			diff:        map[string]any{"type": "circle", "geo_json": "{}", "status": "archived"},
			wantActions: []string{actionGeometryUpdated, actionStatusChanged},
		},
		{
			name:        "empty diff emits nothing",
			diff:        map[string]any{},
			wantActions: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := &captureLogger{}
			LogGeofenceUpdated(context.Background(), l,
				Actor{UserID: uuid.New()},
				Resource{ID: uuid.New(), AgencyID: uuid.New()},
				tc.diff,
			)

			if len(l.reqs) != len(tc.wantActions) {
				t.Fatalf("Log was called %d times, want %d (%v)", len(l.reqs), len(tc.wantActions), tc.wantActions)
			}
			for _, wantAction := range tc.wantActions {
				req := l.action(wantAction)
				if req == nil {
					t.Fatalf("no single request with Action %q among %d captured", wantAction, len(l.reqs))
				}
				if req.GetCategory() == 0 && wantAction != actionUpdated {
					// every non-generic action here maps to a non-zero
					// category (UPDATED=11 or SETTINGS_CHANGED=5); only a
					// real regression would leave it at the zero value.
					t.Errorf("Action %q has zero-value Category", wantAction)
				}
				if req.GetDetails() == "" {
					t.Errorf("Action %q: Details was not set from the diff", wantAction)
				}
			}
		})
	}
}

func TestLogGeofenceUpdated_CategoriesMatchSpec(t *testing.T) {
	l := &captureLogger{}
	LogGeofenceUpdated(context.Background(), l,
		Actor{UserID: uuid.New()},
		Resource{ID: uuid.New(), AgencyID: uuid.New()},
		map[string]any{
			"name":                    "New Name",
			"type":                    "polygon",
			"geo_json":                "{}",
			"status":                  "inactive",
			"exclude_from_colocation": true,
		},
	)

	want := map[string]pb.EventCategory{
		actionUpdated:                    pb.EventCategory_UPDATED,
		actionGeometryUpdated:            pb.EventCategory_UPDATED,
		actionStatusChanged:              pb.EventCategory_SETTINGS_CHANGED,
		actionColocationExclusionChanged: pb.EventCategory_SETTINGS_CHANGED,
	}
	if len(l.reqs) != len(want) {
		t.Fatalf("Log was called %d times, want %d", len(l.reqs), len(want))
	}
	for action, wantCategory := range want {
		req := l.action(action)
		if req == nil {
			t.Fatalf("no single request with Action %q", action)
		}
		if req.GetCategory() != wantCategory {
			t.Errorf("Action %q: Category = %v, want %v", action, req.GetCategory(), wantCategory)
		}
	}
}

func TestLogGeofenceDeleted(t *testing.T) {
	l := &captureLogger{}
	geofenceID := uuid.New()

	LogGeofenceDeleted(context.Background(), l,
		Actor{UserID: uuid.New()},
		Resource{ID: geofenceID, AgencyID: uuid.New()},
	)

	if len(l.reqs) != 1 {
		t.Fatalf("Log was called %d times, want 1", len(l.reqs))
	}
	req := l.reqs[0]

	if req.GetAction() != actionDeleted {
		t.Errorf("Action = %q, want %q", req.GetAction(), actionDeleted)
	}
	if req.GetCategory() != pb.EventCategory_DELETED {
		t.Errorf("Category = %v, want DELETED", req.GetCategory())
	}
	if req.GetMessage() != messageDeleted {
		t.Errorf("Message = %q, want %q", req.GetMessage(), messageDeleted)
	}
	if req.GetResource().GetId() != geofenceID.String() {
		t.Errorf("Resource.Id = %q, want %q", req.GetResource().GetId(), geofenceID.String())
	}
}

// TestLogGeofenceCreated_NilLogger confirms a nil Logger (an uninitialized
// GeofenceController.activityLog, e.g. in a test built without going
// through NewLogger) never panics — it's silently treated as a no-op.
func TestLogGeofenceCreated_NilLogger(t *testing.T) {
	LogGeofenceCreated(context.Background(), nil, Actor{UserID: uuid.New()}, Resource{ID: uuid.New(), AgencyID: uuid.New()}, nil)
}
