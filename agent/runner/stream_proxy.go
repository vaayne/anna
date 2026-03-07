package runner

import (
	"encoding/json"
	"fmt"

	aitypes "github.com/vaayne/anna/ai/types"
)

// Envelope wraps stream events for transport.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// EncodeEvent serializes normalized assistant events.
func EncodeEvent(event aitypes.AssistantEvent) ([]byte, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	env := Envelope{Type: eventName(event), Data: payload}
	return json.Marshal(env)
}

// DecodeEvent deserializes envelope to concrete event type.
func DecodeEvent(raw []byte) (aitypes.AssistantEvent, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}

	switch env.Type {
	case "textDelta":
		var e aitypes.EventTextDelta
		if err := json.Unmarshal(env.Data, &e); err != nil {
			return nil, err
		}
		return e, nil
	case "stop":
		var e aitypes.EventStop
		if err := json.Unmarshal(env.Data, &e); err != nil {
			return nil, err
		}
		return e, nil
	case "usage":
		var e aitypes.EventUsage
		if err := json.Unmarshal(env.Data, &e); err != nil {
			return nil, err
		}
		return e, nil
	default:
		return nil, fmt.Errorf("unsupported event type %q", env.Type)
	}
}

func eventName(event aitypes.AssistantEvent) string {
	switch event.(type) {
	case aitypes.EventTextDelta:
		return "textDelta"
	case aitypes.EventStop:
		return "stop"
	case aitypes.EventUsage:
		return "usage"
	default:
		return "unknown"
	}
}
