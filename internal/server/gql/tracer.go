package gql

import (
	"context"
	"strings"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/ast"
	"go.uber.org/zap"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/tracing"
)

type loggingTracer struct{}

var _ interface {
	graphql.HandlerExtension
	graphql.ResponseInterceptor
} = &loggingTracer{}

func (t *loggingTracer) ExtensionName() string {
	return "logging_tracer"
}

func (t *loggingTracer) Validate(schema graphql.ExecutableSchema) error {
	return nil
}

func (t *loggingTracer) InterceptResponse(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
	if graphql.HasOperationContext(ctx) {
		opCtx := graphql.GetOperationContext(ctx)
		ctx = tracing.WithOperationName(ctx, opCtx.OperationName)

		if log.DebugEnabled(ctx) {
			if shouldOmitGraphQLDebugPayload(opCtx) {
				// Never log raw query text or variables for mutations / credential-bearing
				// operations (prevents leaking secrets via block strings, variable defaults,
				// or inline literals even on rejected mutations).
				log.Debug(ctx, "received graphql request",
					zap.String("operation", opCtx.OperationName),
					zap.Bool("payload_redacted", true),
				)
			} else {
				log.Debug(ctx, "received graphql request",
					zap.String("operation", opCtx.OperationName),
					zap.Any("variables", redactGraphQLValue(opCtx.Variables)),
				)
			}
		}
	}

	resp := next(ctx)

	// Capture GraphQL errors to context for access logging
	if resp != nil && len(resp.Errors) > 0 {
		for _, gqlErr := range resp.Errors {
			contexts.AddError(ctx, gqlErr)
		}
	}

	return resp
}

func shouldOmitGraphQLDebugPayload(opCtx *graphql.OperationContext) bool {
	if opCtx == nil {
		return true
	}
	if opCtx.Operation != nil && opCtx.Operation.Operation == ast.Mutation {
		return true
	}
	lower := strings.ToLower(opCtx.RawQuery)
	for _, marker := range []string{"mutation", "probeapikey", "apikey", "credentials", "secret", "password", "token"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isSensitiveGraphQLKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	switch lower {
	case "probeapikey", "probe_api_key", "probeapikeyciphertext",
		"apikey", "api_key", "apikeys", "api_keys",
		"secret", "secretkey", "secret_key",
		"password", "accesstoken", "access_token", "refreshtoken", "refresh_token":
		return true
	default:
		return false
	}
}

func redactGraphQLValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, elem := range v {
			if isSensitiveGraphQLKey(k) {
				out[k] = "[REDACTED]"
				continue
			}
			out[k] = redactGraphQLValue(elem)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, elem := range v {
			out[i] = redactGraphQLValue(elem)
		}
		return out
	default:
		return value
	}
}
