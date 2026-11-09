package openai

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/spf13/cast"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TransformOpenAIError(ctx context.Context, rawErr *httpclient.Error) *llm.ResponseError {
	return transformOpenAIErrorWithFormat(ctx, rawErr, llm.APIFormatOpenAIChatCompletion)
}

func transformOpenAIErrorWithFormat(_ context.Context, rawErr *httpclient.Error, format llm.APIFormat) *llm.ResponseError {
	if rawErr == nil {
		return &llm.ResponseError{
			StatusCode: http.StatusInternalServerError,
			Detail: llm.ErrorDetail{
				Message: http.StatusText(http.StatusInternalServerError),
				Type:    "api_error",
			},
		}
	}

	var openaiError struct {
		Error struct {
			Message   string          `json:"message"`
			Type      string          `json:"type"`
			Param     string          `json:"param,omitempty"`
			Code      any             `json:"code"`
			RequestID string          `json:"request_id,omitempty"`
			Details   json.RawMessage `json:"details,omitempty"`
		} `json:"error"`
		Errors struct {
			Message   string          `json:"message"`
			Type      string          `json:"type"`
			Param     string          `json:"param,omitempty"`
			Code      any             `json:"code"`
			RequestID string          `json:"request_id,omitempty"`
			Details   json.RawMessage `json:"details,omitempty"`
		} `json:"errors"`
		RequestID string `json:"request_id,omitempty"`
	}

	err := json.Unmarshal(rawErr.Body, &openaiError)
	if err == nil && (openaiError.Error.Message != "" || openaiError.Errors.Message != "") {
		errDetail := openaiError.Error
		if errDetail.Message == "" {
			errDetail = openaiError.Errors
		}

		reqID := errDetail.RequestID
		if reqID == "" {
			reqID = openaiError.RequestID
		}

		return llm.EnrichResponseError(&llm.ResponseError{
			StatusCode: rawErr.StatusCode,
			Detail: llm.ErrorDetail{
				Message:   errDetail.Message,
				Type:      errDetail.Type,
				Param:     errDetail.Param,
				Code:      cast.ToString(errDetail.Code),
				RequestID: reqID,
				Details:   errDetail.Details,
			},
		}, rawErr, format)
	}

	if extracted, ok := llm.ExtractUpstreamErrorDetail(rawErr.Body, rawErr.Headers); ok && extracted.Message != "" {
		if extracted.Type == "" {
			extracted.Type = "api_error"
		}
		return llm.EnrichResponseError(&llm.ResponseError{
			StatusCode: rawErr.StatusCode,
			Detail:     extracted,
		}, rawErr, format)
	}

	return llm.EnrichResponseError(&llm.ResponseError{
		StatusCode: rawErr.StatusCode,
		Detail: llm.ErrorDetail{
			Message: http.StatusText(rawErr.StatusCode),
			Type:    "api_error",
		},
	}, rawErr, format)
}
