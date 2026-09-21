package handler

import (
	"context"
	"net/http"
	"strings"

	commonlog "github.com/sentinelgo/synergy-common/pkg/log"
)

func setCorrelationIDHeader(ctx context.Context, req *http.Request) {
	if ctx == nil || req == nil {
		return
	}
	if corrID := strings.TrimSpace(commonlog.CorrelationIdFromContext(ctx)); corrID != "" {
		req.Header.Set(commonlog.HeaderCorrelationId, corrID)
	}
}
