package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/looplj/axonhub/llm"
)

func renderAgentMessage(item Item) string {
	var text strings.Builder
	if item.Author != "" || item.Recipient != "" {
		fmt.Fprintf(&text, "Agent message from %s to %s:\n", item.Author, item.Recipient)
	}
	if item.Content == nil {
		return text.String()
	}
	if item.Content.Text != nil {
		text.WriteString(*item.Content.Text)
	}
	for _, part := range item.Content.Items {
		switch part.Type {
		case "input_text", "text":
			if part.Text != nil {
				text.WriteString(*part.Text)
			}
		case "encrypted_content":
			// Codex's legacy inter-agent envelope stores its supplied string
			// verbatim here, even when it is plaintext. Forward the literal
			// value; this does not decrypt or reinterpret an opaque payload.
			if part.EncryptedContent != nil {
				text.WriteString(*part.EncryptedContent)
			}
		}
	}
	return text.String()
}

func originalAgentMessage(msg llm.Message) (Item, bool) {
	if len(msg.ResponsesAgentMessage) == 0 || msg.Role != "user" || msg.Content.Content == nil {
		return Item{}, false
	}
	var item Item
	if json.Unmarshal(msg.ResponsesAgentMessage, &item) != nil {
		return Item{}, false
	}
	if item.Type != "agent_message" || renderAgentMessage(item) != *msg.Content.Content {
		return Item{}, false
	}
	return item, true
}
