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

const agencyTreePathFormat = "/api/v1/m2m/agencies/%s/tree"

type agencyClient struct {
	baseURL string
	cli     *http.Client
}

type agencyHierarchyResponse struct {
	Data *agencyHierarchyNode `json:"data"`
}

type agencyHierarchyNode struct {
	ID       uuid.UUID              `json:"id"`
	Children []*agencyHierarchyNode `json:"children"`
}

// newAgencyClient wraps the agency service's m2m tree endpoint
// (FetchHierarchy). cli must be a client_credentials-authenticated
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
