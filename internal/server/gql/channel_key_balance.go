package gql

import (
	"encoding/json"

	"github.com/looplj/axonhub/internal/objects"
)

func marshalChannelKeyBalance(value any) (objects.JSONRawMessage, error) {
	if value == nil {
		return nil, nil
	}

	switch v := value.(type) {
	case objects.JSONRawMessage:
		return v, nil
	case json.RawMessage:
		return objects.JSONRawMessage(v), nil
	case []byte:
		return objects.JSONRawMessage(v), nil
	case string:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}

		return objects.JSONRawMessage(data), nil
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}

		return objects.JSONRawMessage(data), nil
	}
}

func unmarshalChannelKeyBalance(data objects.JSONRawMessage, target *any) error {
	if target == nil {
		return nil
	}
	if data == nil {
		*target = nil
		return nil
	}

	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	*target = value
	return nil
}
