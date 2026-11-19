package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer"
)

const promptProtectionRejectedMessage = "request blocked by prompt protection policy"

func protectPrompts(inbound *PersistentInboundTransformer) pipeline.Middleware {
	return pipeline.OnLlmRequest("protect-prompts", func(ctx context.Context, llmRequest *llm.Request) (*llm.Request, error) {
		if llmRequest.RequestType == llm.RequestTypeDecision || inbound.state.PromptProtecter == nil {
			return llmRequest, nil
		}

		if inbound.state.llmRequestShallow {
			if checker, ok := inbound.state.PromptProtecter.(interface {
				HasEnabledRules(context.Context) (bool, error)
			}); ok {
				hasRules, err := checker.HasEnabledRules(ctx)
				if err != nil {
					log.Warn(ctx, "failed to check prompt protection rules", log.Cause(err))
					return llmRequest, nil
				}
				if !hasRules {
					return llmRequest, nil
				}
			}
			if _, err := inbound.state.EnsureFullLlmRequest(ctx); err != nil {
				return nil, err
			}
		}

		var (
			protected *llm.Request
			masked    bool
			err       error
		)
		if withStatus, ok := inbound.state.PromptProtecter.(interface {
			ProtectWithStatus(context.Context, *llm.Request) (*llm.Request, bool, error)
		}); ok {
			protected, masked, err = withStatus.ProtectWithStatus(ctx, llmRequest)
		} else {
			protected, err = inbound.state.PromptProtecter.Protect(ctx, llmRequest)
			masked = err == nil && protected != nil && protected != llmRequest
		}
		if err != nil {
			if errors.Is(err, biz.ErrPromptProtectionRejected) {
				return nil, fmt.Errorf("%w: %s", transformer.ErrInvalidRequest, promptProtectionRejectedMessage)
			}

			log.Warn(ctx, "failed to protect prompts", log.Cause(err))

			return llmRequest, nil
		}

		if masked {
			inbound.state.ASTContentMutated = true
		}

		if protected == nil {
			return llmRequest, nil
		}

		return protected, nil
	})
}
