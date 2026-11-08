package muse

// Ported from executeMessagesStream in the fork meta_messages_executor.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"io"
	"strings"
)

func (s *messagesSurface) pumpMessages(ctx context.Context, call *Call, body io.ReadCloser, metering *MetaMessagesMetering, sink func([]byte) error) error {
	reader := NewMetaSSEReader(body)
	started := false
	openBlocks := map[int64]string{}
	inputFragments := map[int64]*strings.Builder{}
	nextIndex := int64(0)
	sawMessageDelta := false
	for {
		frame, data, err := reader.Next()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == io.EOF {
				err = messagesGatewayError("truncated_stream", "upstream ended before message_stop")
			}
			return err
		}
		if len(data) == 0 {
			if err := sink(frame); err != nil {
				return err
			}
			continue
		}
		root, err := DecodeMetaMessages(data)
		if err != nil {
			return messagesGatewayError("invalid_stream", "upstream SSE data is not a JSON object")
		}
		typ, _ := root["type"].(string)
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			return messagesGatewayError("invalid_stream", "invalid SSE JSON")
		}
		data = compact.Bytes()
		frame = append(append([]byte("event: "+typ+"\ndata: "), compact.Bytes()...), []byte("\n\n")...)
		if typ == "error" {
			return messagesStreamError(data)
		}
		switch typ {
		case "ping":
		case "message_start":
			if started {
				return messagesGatewayError("invalid_stream", "duplicate message_start")
			}
			message, ok := root["message"].(map[string]any)
			if !ok || message["type"] != "message" || message["role"] != "assistant" {
				return messagesGatewayError("invalid_stream", "invalid message_start")
			}
			initial, ok := message["content"].([]any)
			if !ok || len(initial) != 0 {
				return messagesGatewayError("invalid_stream", "message_start must contain an empty content array")
			}
			started = true

			metering.Observe(data, false)
			alias := messagesAlias(call)
			if alias != "" {
				data, _ = sjson.SetBytes(data, "message.model", alias)
				frame = append(append([]byte("event: message_start\ndata: "), data...), []byte("\n\n")...)
			}
		case "content_block_start":
			index, validIndex := messagesEventIndex(root)
			if !started || sawMessageDelta || !validIndex || index != nextIndex || len(openBlocks) >= 128 {
				return messagesGatewayError("invalid_stream", "invalid content block index or order")
			}
			block, ok := root["content_block"].(map[string]any)
			if !ok {
				return messagesGatewayError("invalid_stream", "missing content block")
			}
			if err := ValidateMetaMessagesResponseBlock(block, s.exec.messagesPolicy(call.Model), true); err != nil {
				return messagesGatewayError("unmapped_block", "upstream emitted an unsupported content block")
			}
			openBlocks[index], _ = block["type"].(string)
			nextIndex++
		case "content_block_delta":
			index, validIndex := messagesEventIndex(root)
			blockType, exists := openBlocks[index]
			if !validIndex || !exists {
				return messagesGatewayError("invalid_stream", "delta has no open content block")
			}
			deltaType := gjson.GetBytes(data, "delta.type").String()
			valid := (blockType == "text" && (deltaType == "text_delta" || deltaType == "citations_delta")) || ((blockType == "tool_use" || blockType == "server_tool_use") && deltaType == "input_json_delta") || (blockType == "thinking" && (deltaType == "thinking_delta" || deltaType == "signature_delta"))
			if !valid {
				return messagesGatewayError("unmapped_delta", "upstream delta does not match an enabled content type")
			}
			delta, _ := root["delta"].(map[string]any)
			field := "text"
			switch deltaType {
			case "thinking_delta":
				field = "thinking"
			case "signature_delta":
				field = "signature"
			case "input_json_delta":
				field = "partial_json"
			case "citations_delta":
				field = "citation"
			}
			if deltaType == "citations_delta" {
				if _, ok := delta[field].(map[string]any); !ok {
					return messagesGatewayError("invalid_stream", "citation delta is not an object")
				}
			} else if value, ok := delta[field].(string); !ok {
				return messagesGatewayError("invalid_stream", "delta payload is not a string")
			} else if deltaType == "input_json_delta" {
				fragments := inputFragments[index]
				if fragments == nil {
					fragments = &strings.Builder{}
					inputFragments[index] = fragments
				}
				if fragments.Len()+len(value) > 16<<20 {
					return messagesGatewayError("invalid_stream", "streamed tool arguments exceed 16 MiB")
				}
				fragments.WriteString(value)
			}
		case "content_block_stop":
			index, validIndex := messagesEventIndex(root)
			if _, exists := openBlocks[index]; !exists || !validIndex {
				return messagesGatewayError("invalid_stream", "stop has no open content block")
			}
			if fragments, exists := inputFragments[index]; exists {
				if _, err := DecodeMetaMessages([]byte(fragments.String())); fragments.Len() > 0 && err != nil {
					return messagesGatewayError("invalid_tool_json", "completed tool arguments are not a valid JSON object")
				}
				delete(inputFragments, index)
			}
			delete(openBlocks, index)
		case "message_delta":
			if !started || len(openBlocks) > 0 {
				return messagesGatewayError("invalid_stream", "message_delta arrived outside the message boundary")
			}

			metering.Observe(data, true)
			sawMessageDelta = true
		case "message_stop":
			if !started || !sawMessageDelta || len(openBlocks) > 0 {
				return messagesGatewayError("invalid_stream", "message_stop arrived before content completion")
			}
			if err := sink(frame); err != nil {
				return err
			}
			return nil
		default:
			return messagesGatewayError("unmapped_event", "upstream emitted an unsupported SSE event")
		}
		if MetaMessagesSSEUsefulProgress(data) {
			MetaMessagesURLImageProgress(body)
		}
		if err := sink(frame); err != nil {
			return err
		}
	}
}
