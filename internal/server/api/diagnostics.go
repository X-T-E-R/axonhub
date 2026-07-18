package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/server/diagnostics"
	contractv1 "github.com/looplj/axonhub/internal/server/diagnostics/contract/v1"
)

const diagnosticsMediaType = diagnostics.ContractMediaType

type DiagnosticsHandlersParams struct {
	fx.In
	Service *diagnostics.Service
}
type DiagnosticsHandlers struct{ service *diagnostics.Service }

func NewDiagnosticsHandlers(p DiagnosticsHandlersParams) *DiagnosticsHandlers {
	return &DiagnosticsHandlers{service: p.Service}
}

func (h *DiagnosticsHandlers) Pull(c *gin.Context) {
	setDiagnosticsHeaders(c)
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.writeError(c, http.StatusBadRequest, "INVALID_CONTENT_TYPE", "Content-Type must be application/json", false, false)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	contractRequest, err := diagnostics.DecodeContractPullRequest(c.Request.Body)
	if err != nil {
		status := http.StatusBadRequest
		code := "VALIDATION_FAILED"
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			status = http.StatusRequestEntityTooLarge
			code = "REQUEST_TOO_LARGE"
		} else if errors.Is(err, io.ErrUnexpectedEOF) {
			code = "INVALID_JSON"
		}
		h.writeError(c, status, code, "request body is invalid", false, false)
		return
	}
	req, err := diagnostics.PullRequestFromContract(contractRequest)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, "VALIDATION_FAILED", "request body is invalid", false, false)
		return
	}
	response, err := h.service.Pull(c.Request.Context(), req)
	if err != nil {
		var serviceErr *diagnostics.ServiceError
		if errors.As(err, &serviceErr) {
			h.writeError(c, serviceErr.Status, serviceErr.Code, serviceErr.Message, serviceErr.Retryable, serviceErr.Supported)
			return
		}
		h.writeError(c, http.StatusServiceUnavailable, "CORE_DATABASE_UNAVAILABLE", "diagnostic pull is temporarily unavailable", true, false)
		return
	}
	contractResponse, err := diagnostics.PullResponseToContract(response)
	if err != nil {
		h.writeError(c, http.StatusServiceUnavailable, "SERIALIZATION_FAILED", "diagnostic response did not match the contract schema", false, false)
		return
	}
	raw, err := json.Marshal(contractResponse)
	if err != nil {
		h.writeError(c, http.StatusServiceUnavailable, "SERIALIZATION_FAILED", "diagnostic response could not be serialized", false, false)
		return
	}
	if err := diagnostics.ValidatePullResponseJSON(raw); err != nil {
		h.writeError(c, http.StatusServiceUnavailable, "SERIALIZATION_FAILED", "diagnostic response did not match the contract schema", false, false)
		return
	}
	c.Data(http.StatusOK, diagnosticsMediaType, raw)
}

func (h *DiagnosticsHandlers) writeError(c *gin.Context, status int, code, message string, retryable, supported bool) {
	setDiagnosticsHeaders(c)
	ranges := []contractv1.SupportedRange{}
	if supported {
		ranges = append(ranges, contractv1.SupportedRange{Major: diagnostics.ContractMajor, MinMinor: diagnostics.ContractMinorMinimum, MaxMinor: diagnostics.ContractMinor})
	}
	payload := contractv1.ErrorResponse{Contract: contractv1.ContractResponse{Name: diagnostics.ContractName, Major: diagnostics.ContractMajor, Minor: diagnostics.ContractMinor, SchemaSha256: diagnostics.SchemaSHA256}, Error: contractv1.ErrorDetail{Code: code, Message: message, CorrelationID: uuid.NewString(), Retryable: retryable, Supported: ranges}}
	raw, err := json.Marshal(payload)
	if err != nil || diagnostics.ValidateErrorResponseJSON(raw) != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Data(status, diagnosticsMediaType, raw)
}

// MiddlewareError keeps authentication and project-header failures on the
// diagnostics transport contract even though they occur before Pull executes.
func (h *DiagnosticsHandlers) MiddlewareError(c *gin.Context, status int, _ error) {
	code := "UNAUTHENTICATED"
	message := "authentication is required"
	retryable := false
	if status == http.StatusBadRequest {
		code = "INVALID_PROJECT"
		message = "X-Project-ID is invalid"
	} else if status >= http.StatusInternalServerError {
		status = http.StatusServiceUnavailable
		code = "AUTHENTICATION_UNAVAILABLE"
		message = "authentication is temporarily unavailable"
		retryable = true
	}
	h.writeError(c, status, code, message, retryable, false)
}

func setDiagnosticsHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
}
