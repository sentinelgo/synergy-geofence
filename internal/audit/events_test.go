package audit

import (
	"context"
	"fmt"
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
		Actor{UserID: userID, IP: "10.0.0.1", Role: "agencyadmin"},
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
	wantMessage := "Agency Configuration: Known Location Downtown Office Added"
	if req.GetMessage() != wantMessage {
		t.Errorf("Message = %q, want %q", req.GetMessage(), wantMessage)
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
	if req.Metadata["role"] != "agencyadmin" {
		t.Errorf(`Metadata["role"] = %q, want %q`, req.Metadata["role"], "agencyadmin")
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
		name                       string
		changes                    []FieldChange
		colocationExclusionChanged *bool
		wantActions                []string
	}{
		{
			name:        "generic detail field only",
			changes:     []FieldChange{{Field: "Name", Old: "Old Name", New: "New Name"}},
			wantActions: []string{actionUpdated},
		},
		{
			name:        "geometry only",
			changes:     []FieldChange{{Field: "Location", Old: "Point", New: "Polygon"}},
			wantActions: []string{actionGeometryUpdated},
		},
		{
			name:        "status only",
			changes:     []FieldChange{{Field: "Status", Old: "active", New: "inactive"}},
			wantActions: []string{actionStatusChanged},
		},
		{
			name:                       "colocation exclusion only",
			colocationExclusionChanged: ptr(true),
			wantActions:                []string{actionColocationExclusionChanged},
		},
		{
			name: "geometry and status together emit two distinct events",
			changes: []FieldChange{
				{Field: "Location", Old: "Point", New: "Circle"},
				{Field: "Status", Old: "active", New: "archived"},
			},
			wantActions: []string{actionGeometryUpdated, actionStatusChanged},
		},
		{
			name:        "no changes emits nothing",
			wantActions: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := &captureLogger{}
			LogGeofenceUpdated(context.Background(), l,
				Actor{UserID: uuid.New()},
				Resource{ID: uuid.New(), Name: "Downtown Office", AgencyID: uuid.New()},
				tc.changes, tc.colocationExclusionChanged, tc.changes,
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
				if len(tc.changes) > 0 && req.GetDetails() == "" {
					t.Errorf("Action %q: Details was not set from the changes", wantAction)
				}
			}
		})
	}
}

// TestLogGeofenceUpdated_MessageText locks down the exact "Known Location
// <Name> Modified <Field> from <Old> to <New>" text per changed field.
func TestLogGeofenceUpdated_MessageText(t *testing.T) {
	l := &captureLogger{}
	LogGeofenceUpdated(context.Background(), l,
		Actor{UserID: uuid.New()},
		Resource{ID: uuid.New(), Name: "Downtown Office", AgencyID: uuid.New()},
		[]FieldChange{
			{Field: "Name", Old: "Old Name", New: "New Name"},
			{Field: "Status", Old: "active", New: "inactive"},
		}, nil, nil,
	)

	want := map[string]string{
		actionUpdated:       "Agency Configuration: Known Location Downtown Office Modified Name from Old Name to New Name",
		actionStatusChanged: "Agency Configuration: Known Location Downtown Office Modified Status from active to inactive",
	}
	if len(l.reqs) != len(want) {
		t.Fatalf("Log was called %d times, want %d", len(l.reqs), len(want))
	}
	for action, wantMessage := range want {
		req := l.action(action)
		if req == nil {
			t.Fatalf("no single request with Action %q", action)
		}
		if req.GetMessage() != wantMessage {
			t.Errorf("Action %q: Message = %q, want %q", action, req.GetMessage(), wantMessage)
		}
	}
}

// TestLogGeofenceUpdated_ColocationMessageText locks down the Excluded/
// Included wording, which — unlike every other field — has no "from <Old>
// to <New>" clause: it just flips between two fixed sentences based on the
// flag's new value.
func TestLogGeofenceUpdated_ColocationMessageText(t *testing.T) {
	tests := []struct {
		excluded    bool
		wantMessage string
	}{
		{excluded: true, wantMessage: "Agency Configuration: Known Location Downtown Office Excluded from Co-Location Report."},
		{excluded: false, wantMessage: "Agency Configuration: Known Location Downtown Office Included for Co-Location Report."},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("excluded=%v", tc.excluded), func(t *testing.T) {
			l := &captureLogger{}
			LogGeofenceUpdated(context.Background(), l,
				Actor{UserID: uuid.New()},
				Resource{ID: uuid.New(), Name: "Downtown Office", AgencyID: uuid.New()},
				nil, ptr(tc.excluded), nil,
			)

			if len(l.reqs) != 1 {
				t.Fatalf("Log was called %d times, want 1", len(l.reqs))
			}
			req := l.reqs[0]
			if req.GetAction() != actionColocationExclusionChanged {
				t.Errorf("Action = %q, want %q", req.GetAction(), actionColocationExclusionChanged)
			}
			if req.GetCategory() != pb.EventCategory_SETTINGS_CHANGED {
				t.Errorf("Category = %v, want SETTINGS_CHANGED", req.GetCategory())
			}
			if req.GetMessage() != tc.wantMessage {
				t.Errorf("Message = %q, want %q", req.GetMessage(), tc.wantMessage)
			}
		})
	}
}

func TestLogGeofenceUpdated_CategoriesMatchSpec(t *testing.T) {
	l := &captureLogger{}
	LogGeofenceUpdated(context.Background(), l,
		Actor{UserID: uuid.New()},
		Resource{ID: uuid.New(), Name: "Downtown Office", AgencyID: uuid.New()},
		[]FieldChange{
			{Field: "Name", Old: "Old Name", New: "New Name"},
			{Field: "Location", Old: "Point", New: "Polygon"},
			{Field: "Status", Old: "active", New: "inactive"},
		},
		ptr(true),
		nil,
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
		Resource{ID: geofenceID, Name: "Downtown Office", AgencyID: uuid.New()},
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
	wantMessage := "Agency Configuration: Known Location Downtown Office Deleted"
	if req.GetMessage() != wantMessage {
		t.Errorf("Message = %q, want %q", req.GetMessage(), wantMessage)
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

// TestLogGeofenceCreated_NoRoleOmitsMetadataKey guards log1's
// mirror of synergy-sidecar's WithRole: an unresolved Role (e.g. JWT
// verification disabled, so requireAgencyAdmin never ran) must leave the
// "role" key out of Metadata entirely, not set it to "".
func TestLogGeofenceCreated_NoRoleOmitsMetadataKey(t *testing.T) {
	l := &captureLogger{}
	LogGeofenceCreated(context.Background(), l,
		Actor{UserID: uuid.New()},
		Resource{ID: uuid.New(), AgencyID: uuid.New()},
		nil,
	)

	req := l.reqs[0]
	if _, ok := req.Metadata["role"]; ok {
		t.Errorf(`Metadata["role"] = %q, want key absent`, req.Metadata["role"])
	}
}

// ptr is a small local helper for building the *bool colocationExclusionChanged
// arg inline in table-driven tests, above.
func ptr[T any](v T) *T { return &v }
