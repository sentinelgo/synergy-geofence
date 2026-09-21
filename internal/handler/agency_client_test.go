package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/sentinelgo/synergy-common/pkg/config"
)

func TestFetchAgencyDetails_CachesResult(t *testing.T) {
	agencyID := uuid.New()
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		wantPath := "/api/v1/m2m/agencies/" + agencyID.String() + "/profile"
		if r.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"synergy_identifier": "SYN123"},
		})
	}))
	defer server.Close()

	cfg := &config.Config{Common: config.Common{Agency: config.Agency{BaseUrl: server.URL}}}
	cache := newHomeAgencyCache()
	const wantSynergyID = "SYN123"

	for i := 0; i < 3; i++ {
		client, err := newAgencyClient(cfg, server.Client(), cache)
		if err != nil {
			t.Fatalf("newAgencyClient: %v", err)
		}

		synergyID, err := client.FetchAgencyDetails(context.Background(), agencyID)
		if err != nil {
			t.Fatalf("FetchAgencyDetails: %v", err)
		}
		if synergyID != wantSynergyID {
			t.Fatalf("synergyID = %q, want %q", synergyID, wantSynergyID)
		}
		// ristretto's Set is processed asynchronously — Wait() ensures the
		// write from this iteration is visible before the next Get.
		cache.Wait()
	}

	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("agency service received %d requests, want 1 (subsequent lookups should be served from cache)", got)
	}
}

func TestFetchAgencyDetails_RejectsMissingSynergyIdentifier(t *testing.T) {
	agencyID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	}))
	defer server.Close()

	cfg := &config.Config{Common: config.Common{Agency: config.Agency{BaseUrl: server.URL}}}
	client, err := newAgencyClient(cfg, server.Client(), newHomeAgencyCache())
	if err != nil {
		t.Fatalf("newAgencyClient: %v", err)
	}

	if _, err := client.FetchAgencyDetails(context.Background(), agencyID); err == nil {
		t.Fatal("FetchAgencyDetails: want error for a response missing synergy_identifier")
	}
}
