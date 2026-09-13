package openai

import (
	"fmt"

	"github.com/samber/lo"
)

// Chat 工具结果只支持文本；跨协议图片保留在随后 user 消息中，
// 原位置用编号标记关联。原生 Chat 的扩展协议不经过这里。
func liftToolResultImages(messages []Message) []Message {
	result := make([]Message, 0, len(messages))
	var images []MessageContentPart
	flushImages := func() {
		if len(images) > 0 {
			result = append(result, Message{
				Role:    "user",
				Content: MessageContent{MultipleContent: images},
			})
			images = nil
		}
	}

	for _, message := range messages {
		if message.Role != "tool" {
			// 并行调用的 tool 结果必须连续，不能在中间插入 user。
			flushImages()
		}
		if message.Role == "tool" {
			var parts []MessageContentPart
			imageNumber := 0
			for index, part := range message.Content.MultipleContent {
				if part.Type != "image_url" || part.ImageURL == nil {
					if parts != nil {
						parts = append(parts, part)
					}
					continue
				}
				if parts == nil {
					parts = make([]MessageContentPart, 0, len(message.Content.MultipleContent))
					parts = append(parts, message.Content.MultipleContent[:index]...)
				}
				imageNumber++
				label := fmt.Sprintf("Tool result %q, image %d", lo.FromPtr(message.ToolCallID), imageNumber)
				parts = append(parts, MessageContentPart{
					Type: "text",
					Text: lo.ToPtr("[" + label + " attached below]"),
				})
				images = append(images,
					MessageContentPart{Type: "text", Text: lo.ToPtr(label + ":")},
					part,
				)
			}
			if parts != nil {
				message.Content = MessageContent{MultipleContent: parts}
			}
		}
		result = append(result, message)
	}
	flushImages()
	return result
}
