package pipeline

import (
	"context"
	"errors"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// UpstreamError marks errors that originate from the upstream provider path.
type UpstreamError struct {
	Err error
}

func (e *UpstreamError) Error() string {
	if e == nil || e.Err == nil {
		return "upstream error"
	}

	return e.Err.Error()
}

func (e *UpstreamError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

func WrapUpstreamError(err error) error {
	if err == nil {
		return nil
	}

	var upstreamErr *UpstreamError
	if errors.As(err, &upstreamErr) {
		return err
	}

	return &UpstreamError{Err: err}
}

func IsUpstreamError(err error) bool {
	var upstreamErr *UpstreamError
	return errors.As(err, &upstreamErr)
}

func (p *pipeline) transformOutboundError(ctx context.Context, request *httpclient.Request, httpErr *httpclient.Error) error {
	apiFormat := p.Outbound.APIFormat()
	if request != nil && request.APIFormat != "" {
		apiFormat = llm.APIFormat(request.APIFormat)
	}

	respErr := p.Outbound.TransformError(ctx, httpErr)
	return WrapUpstreamError(llm.EnrichResponseError(respErr, httpErr, apiFormat))
}
