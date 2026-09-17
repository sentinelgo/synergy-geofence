package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/google/uuid"
	"github.com/sentinelgo/synergy-common/pkg/config"
	http2 "github.com/sentinelgo/synergy-common/pkg/http"
)

// homeAgencyCacheTTL mirrors sso-user-service's agency-lookup cache TTL
// (see its pkg/service/agency.agencyService) — long enough to spare the
// agency service a round trip on every geofence write, short enough that a
// resynced synergy_identifier shows up on newly written/updated rows within
// minutes.
const homeAgencyCacheTTL = 5 * time.Minute

const (
	agencyTreePathFormat = "/api/v1/m2m/agencies/%s/tree"
	// agencyProfilePathFormat mirrors agencyTreePathFormat's m2m path shape
	// (see FetchAgencyDetails) — this endpoint has no other confirmed source
	// (the agency-service API isn't checked out locally), so it's inferred
	// from the tree endpoint's own naming convention; verify against the
	// agency-service's actual OpenAPI spec if this 404s in practice.
	agencyProfilePathFormat = "/api/v1/m2m/agencies/%s/profile"
)

type agencyClient struct {
	baseURL string
	cli     *http.Client
	cache   *ristretto.Cache
}

type agencyHierarchyResponse struct {
	Data *agencyHierarchyNode `json:"data"`
}

type agencyHierarchyNode struct {
	ID       uuid.UUID              `json:"id"`
	Children []*agencyHierarchyNode `json:"children"`
}

type agencyProfileResponse struct {
	Data *agencyProfile `json:"data"`
}

type agencyProfile struct {
	SynergyID string `json:"synergy_identifier"`
}

// newHomeAgencyCache builds the process-lifetime cache backing
// FetchAgencyDetails, sized the same as sso-user-service's agency cache. It's
// created once (see GeofenceController.homeAgencyCache) and handed to every
// short-lived agencyClient constructed per request, so lookups are shared
// across requests rather than reset each time.
func newHomeAgencyCache() *ristretto.Cache {
	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: 10_000_000,
		MaxCost:     500_000_000,
		BufferItems: 64,
	})
	if err != nil {
		panic(err)
	}
	return cache
}

// newAgencyClient wraps the agency service's m2m endpoints (FetchHierarchy,
// FetchAgencyDetails). cli must be a client_credentials-authenticated
// *http.Client (see GeofenceController.m2mAcli, built once at startup via
// config.ClientCredentialsConfig.Client) — this constructor doesn't attach
// any auth itself, it relies entirely on cli's own transport to inject the
// bearer token obtained from Hydra via the agency.read-scoped client
// credentials grant.
func newAgencyClient(cfg *config.Config, cli *http.Client, cache *ristretto.Cache) (*agencyClient, error) {
	if cli == nil {
		return nil, errors.New("agency client is nil")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.Common.Agency.BaseUrl), "/")
	if baseURL == "" {
		return nil, errors.New("agency base url is empty")
	}
	return &agencyClient{
		baseURL: baseURL,
		cli:     cli,
		cache:   cache,
	}, nil
}

func (c *agencyClient) FetchHierarchy(ctx context.Context, agencyID uuid.UUID) ([]uuid.UUID, error) {
	if agencyID == uuid.Nil {
		return nil, errors.New("agency id is required")
	}
	url := c.baseURL + fmt.Sprintf(agencyTreePathFormat, agencyID.String())
	req, err := http.NewRequestWithContext(ctx, http2.MethodList, url, nil)
	if err != nil {
		return nil, err
	}

	setCorrelationIDHeader(ctx, req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("agency tree request failed: %s", resp.Status)
	}

	var payload agencyHierarchyResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Data == nil {
		return nil, errors.New("agency tree response missing data")
	}
	ids := flattenAgencyHierarchy(payload.Data)
	if len(ids) == 0 {
		return nil, errors.New("agency tree response empty")
	}
	return ids, nil
}

// FetchAgencyDetails looks up an agency's synergy_identifier from the agency
// service's m2m profile endpoint, for search/sort under ListGeofences'
// "home_agency" field (see search.MatchesQuery and list_sort.go's
// "home_agency" case, both applied in memory over Geofence.SynergyIdentifier).
// Like FetchHierarchy, this is authorized via c's client_credentials
// transport (see GeofenceController.m2mAcli/config.ClientCredentialsConfig.Client)
// rather than forwarding any caller token — there is no per-user variant of
// this call anymore.
// Results are cached for homeAgencyCacheTTL (see newHomeAgencyCache), same
// as sso-user-service's own agency lookups, so a burst of geofence writes
// against the same agency doesn't round-trip to the agency service each time.
func (c *agencyClient) FetchAgencyDetails(ctx context.Context, agencyID uuid.UUID) (string, error) {
	if agencyID == uuid.Nil {
		return "", errors.New("agency id is required")
	}

	cacheKey := homeAgencyCacheKey(agencyID)
	if c.cache != nil {
		if cached, found := c.cache.Get(cacheKey); found {
			return cached.(string), nil
		}
	}

	url := c.baseURL + fmt.Sprintf(agencyProfilePathFormat, agencyID.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	setCorrelationIDHeader(ctx, req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.cli.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("agency profile request failed: %s", resp.Status)
	}

	var payload agencyProfileResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.Data == nil {
		return "", errors.New("agency profile response missing data")
	}
	synergyID := strings.TrimSpace(payload.Data.SynergyID)
	if synergyID == "" {
		return "", errors.New("agency profile response missing synergy_identifier")
	}

	if c.cache != nil {
		c.cache.SetWithTTL(cacheKey, synergyID, 0, homeAgencyCacheTTL)
	}
	return synergyID, nil
}

func homeAgencyCacheKey(agencyID uuid.UUID) string {
	return "home_agency:" + agencyID.String()
}

func flattenAgencyHierarchy(root *agencyHierarchyNode) []uuid.UUID {
	if root == nil {
		return nil
	}
	seen := make(map[uuid.UUID]struct{})
	stack := []*agencyHierarchyNode{root}
	for len(stack) > 0 {
		last := len(stack) - 1
		node := stack[last]
		stack = stack[:last]
		if node == nil {
			continue
		}
		if node.ID != uuid.Nil {
			seen[node.ID] = struct{}{}
		}
		for _, child := range node.Children {
			if child != nil {
				stack = append(stack, child)
			}
		}
	}

	out := make([]uuid.UUID, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}
