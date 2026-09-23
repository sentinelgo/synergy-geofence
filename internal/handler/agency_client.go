package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/sentinelgo/synergy-common/pkg/config"
	http2 "github.com/sentinelgo/synergy-common/pkg/http"
)

const (
	agencyTreePathFormat = "/api/v1/m2m/agencies/%s/tree"
	agencyPathFormat     = "/api/v1/m2m/agencies/%s"
)

type agencyClient struct {
	baseURL string
	cli     *http.Client
}

// agencyInfo is the slice of an agency-service agency that ListGeofences
// needs: the id (for scope) plus the synergy identifier and name it
// displays and sorts by as the geofence's home agency. Nothing here is
// stored — it's looked up per request.
type agencyInfo struct {
	ID                uuid.UUID `json:"id"`
	Name              string    `json:"name"`
	SynergyIdentifier string    `json:"synergy_identifier"`
}

type agencyHierarchyResponse struct {
	Data *agencyHierarchyNode `json:"data"`
}

// agencyHierarchyNode mirrors sso-agency-service's HierarchyNode
// (pkg/handler/model/agency/hierarchy.go), decoding only what's used.
type agencyHierarchyNode struct {
	agencyInfo
	Children []*agencyHierarchyNode `json:"children"`
}

type agencyResponse struct {
	Data *agencyInfo `json:"data"`
}

// newAgencyClient wraps the agency service's m2m endpoints (FetchHierarchy,
// FetchAgency). cli must be a client_credentials-authenticated
// *http.Client (see GeofenceController.m2mAcli, built once at startup via
// config.ClientCredentialsConfig.Client) — this constructor doesn't attach
// any auth itself, it relies entirely on cli's own transport to inject the
// bearer token obtained from Hydra via the agency.read-scoped client
// credentials grant.
func newAgencyClient(cfg *config.Config, cli *http.Client) (*agencyClient, error) {
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
	}, nil
}

// FetchHierarchy returns agencyID's whole hierarchy (the agency itself plus
// every descendant), flattened and de-duplicated, in no particular order.
func (c *agencyClient) FetchHierarchy(ctx context.Context, agencyID uuid.UUID) ([]agencyInfo, error) {
	var payload agencyHierarchyResponse
	if err := c.getJSON(ctx, http2.MethodList, agencyTreePathFormat, agencyID, &payload); err != nil {
		return nil, fmt.Errorf("agency tree request: %w", err)
	}
	if payload.Data == nil {
		return nil, errors.New("agency tree response missing data")
	}
	agencies := flattenAgencyHierarchy(payload.Data)
	if len(agencies) == 0 {
		return nil, errors.New("agency tree response empty")
	}
	return agencies, nil
}

// FetchAgency returns a single agency (GET /api/v1/m2m/agencies/:id) — the
// lightweight lookup for a single-agency list's home agency, rather than
// pulling a potentially large tree for one row.
func (c *agencyClient) FetchAgency(ctx context.Context, agencyID uuid.UUID) (agencyInfo, error) {
	var payload agencyResponse
	if err := c.getJSON(ctx, http.MethodGet, agencyPathFormat, agencyID, &payload); err != nil {
		return agencyInfo{}, fmt.Errorf("agency request: %w", err)
	}
	if payload.Data == nil {
		return agencyInfo{}, errors.New("agency response missing data")
	}
	info := *payload.Data
	if info.ID == uuid.Nil {
		info.ID = agencyID
	}
	return info, nil
}

// getJSON issues method against pathFormat (filled with agencyID) and
// decodes a 2xx JSON response into out.
func (c *agencyClient) getJSON(ctx context.Context, method, pathFormat string, agencyID uuid.UUID, out any) error {
	if agencyID == uuid.Nil {
		return errors.New("agency id is required")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+fmt.Sprintf(pathFormat, agencyID.String()), nil)
	if err != nil {
		return err
	}

	setCorrelationIDHeader(ctx, req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.cli.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func flattenAgencyHierarchy(root *agencyHierarchyNode) []agencyInfo {
	if root == nil {
		return nil
	}
	seen := make(map[uuid.UUID]agencyInfo)
	stack := []*agencyHierarchyNode{root}
	for len(stack) > 0 {
		last := len(stack) - 1
		node := stack[last]
		stack = stack[:last]
		if node == nil {
			continue
		}
		if node.ID != uuid.Nil {
			seen[node.ID] = node.agencyInfo
		}
		for _, child := range node.Children {
			if child != nil {
				stack = append(stack, child)
			}
		}
	}

	out := make([]agencyInfo, 0, len(seen))
	for _, info := range seen {
		out = append(out, info)
	}
	return out
}
