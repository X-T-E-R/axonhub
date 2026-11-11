package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/server/biz"
)

const maxCompactBatchRequestBytes = 8 << 20

func (h *ClusterHandlers) WithCompactAccounting(compact *biz.CompactAccountingService) *ClusterHandlers {
	if h != nil {
		h.compact = compact
	}
	return h
}

// IngestCompactAccountingBatch receives an HMAC-authenticated compact usage
// batch from a non-control data node and commits it idempotently into the
// central usage_logs ledger + compact_usage_batches receipt table.
func (h *ClusterHandlers) IngestCompactAccountingBatch(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if h == nil || h.compact == nil {
		JSONError(c, http.StatusServiceUnavailable, errors.New("compact accounting receiver unavailable"))
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxCompactBatchRequestBytes)
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		JSONError(c, http.StatusBadRequest, err)
		return
	}

	nodeID := c.GetHeader(biz.HeaderClusterNodeID)
	timestamp := c.GetHeader(biz.HeaderClusterTimestamp)
	signature := c.GetHeader(biz.HeaderClusterSignature)
	if authErr := h.compact.VerifyClusterRequest(c.Request.Context(), nodeID, timestamp, signature, rawBody); authErr != nil {
		JSONError(c, http.StatusUnauthorized, authErr)
		return
	}

	var payload biz.CompactUsageBatchPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		JSONError(c, http.StatusBadRequest, err)
		return
	}

	ack, err := h.compact.IngestBatch(c.Request.Context(), nodeID, payload)
	if err != nil {
		if errors.Is(err, biz.ErrCompactBatchConflict) {
			JSONError(c, http.StatusConflict, err)
			return
		}
		JSONError(c, http.StatusBadRequest, err)
		return
	}

	c.JSON(http.StatusOK, ack)
}
