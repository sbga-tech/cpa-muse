package muse

// Ported from the fork internal/runtime/executor/helps/meta_messages_tokens.go.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer"
)

const (
	MetaMessagesTokenEstimateMethod       = "o200k-semantic-vision-heuristic-v2"
	metaMessageBaseTokens           int64 = 8
	metaMessageFrameTokens          int64 = 4
	MetaUnknownImageTokens          int64 = 1568
	metaUnknownDocumentTokens       int64 = 4096
	metaUnknownVideoTokens          int64 = 8192
	metaImageHeaderBytes                  = 256 << 10
	metaImageDimensionLimit               = 1 << 20
)

// MetaMessagesTokenEstimate exposes heuristic components without claiming provider calibration.
type MetaMessagesTokenEstimate struct {
	InputTokens      int64
	TextTokens       int64
	ImageTokens      int64
	OtherMediaTokens int64
	FramingTokens    int64
	UnknownMedia     int
}

func (e MetaMessagesTokenEstimate) Uncertainty() string {
	if e.UnknownMedia > 0 {
		return "uncalibrated,unknown-media"
	}
	return "uncalibrated"
}

type metaMessagesTokenCounter struct {
	codec  tokenizer.Codec
	detail MetaMessagesTokenEstimate
	err    error
}

// EstimateMetaMessagesTokens uses exactly the same calculation as the count endpoint.
func EstimateMetaMessagesTokens(body []byte) (int64, error) {
	detail, err := EstimateMetaMessagesTokenDetails(body)
	return detail.InputTokens, err
}

// EstimateMetaMessagesTokenDetails counts semantic text, not JSON transport or encoded binary.
func EstimateMetaMessagesTokenDetails(body []byte) (MetaMessagesTokenEstimate, error) {
	root, err := DecodeMetaMessages(body)
	if err != nil {
		return MetaMessagesTokenEstimate{}, err
	}
	codec, err := claudeInputTokenizer()
	if err != nil {
		return MetaMessagesTokenEstimate{}, err
	}
	counter := metaMessagesTokenCounter{codec: codec, detail: MetaMessagesTokenEstimate{FramingTokens: metaMessageBaseTokens}}
	counter.content(root["system"])
	if messages, ok := root["messages"].([]any); ok {
		counter.detail.FramingTokens += int64(len(messages)) * metaMessageFrameTokens
		for _, value := range messages {
			if message, ok := value.(map[string]any); ok {
				counter.content(message["content"])
			}
		}
	}
	if tools, ok := root["tools"].([]any); ok {
		for _, value := range tools {
			if tool, ok := value.(map[string]any); ok {
				counter.text(tool["name"])
				counter.text(tool["description"])
				if schema, exists := tool["input_schema"]; exists {
					counter.json(schema)
				} else {
					counter.text(tool["type"])
				}
			}
		}
	}
	if choice, exists := root["tool_choice"]; exists {
		counter.json(choice)
	}
	if output, ok := root["output_config"].(map[string]any); ok {
		if format, exists := output["format"]; exists {
			counter.json(format)
		}
	}
	counter.detail.InputTokens = counter.detail.TextTokens + counter.detail.ImageTokens + counter.detail.OtherMediaTokens + counter.detail.FramingTokens
	return counter.detail, counter.err
}

func (c *metaMessagesTokenCounter) text(value any) {
	text, ok := value.(string)
	if !ok || text == "" || c.err != nil {
		return
	}
	count, err := c.codec.Count(text)
	if err != nil {
		c.err = err
		return
	}
	c.detail.TextTokens += int64(count)
}

func (c *metaMessagesTokenCounter) json(value any) {
	if value == nil || c.err != nil {
		return
	}
	body, err := json.Marshal(value)
	if err != nil {
		c.err = err
		return
	}
	c.text(string(body))
}

func (c *metaMessagesTokenCounter) content(value any) {
	switch v := value.(type) {
	case string:
		c.text(v)
	case []any:
		for _, block := range v {
			c.content(block)
		}
	case map[string]any:
		switch v["type"] {
		case "text":
			c.text(v["text"])
		case "thinking":
			c.text(v["thinking"])
		case "redacted_thinking":
			// Opaque thinking and signatures cannot be converted to prompt tokens locally.
		case "tool_use", "server_tool_use":
			c.text(v["id"])
			c.text(v["name"])
			c.json(v["input"])
		case "tool_result", "web_search_tool_result", "tool_search_tool_result":
			c.text(v["tool_use_id"])
			c.content(v["content"])
		case "tool_search_tool_search_result":
			if refs, ok := v["tool_references"].([]any); ok {
				for _, ref := range refs {
					c.content(ref)
				}
			}
		case "tool_reference":
			c.text(v["tool_name"])
		case "web_search_result":
			c.text(v["title"])
			c.text(v["url"])
			c.text(v["page_age"])
			c.content(v["content"])
		case "web_search_tool_result_error", "tool_search_tool_result_error":
			c.text(v["error_code"])
		case "image":
			source, _ := v["source"].(map[string]any)
			estimate := EstimateMetaImageSource(source)
			c.detail.ImageTokens += estimate.Tokens
			if !estimate.KnownDimensions {
				c.detail.UnknownMedia++
			}
		case "document":
			c.text(v["title"])
			c.text(v["context"])
			source, _ := v["source"].(map[string]any)
			switch source["type"] {
			case "text":
				c.text(source["data"])
			case "content":
				c.content(source["content"])
			default:
				if source["type"] == "base64" && source["media_type"] == "text/plain" {
					data, _ := source["data"].(string)
					decoded, err := base64.StdEncoding.DecodeString(data)
					if err == nil && utf8.Valid(decoded) {
						c.text(string(decoded))
						return
					}
				}
				c.detail.OtherMediaTokens += metaUnknownDocumentTokens
				c.detail.UnknownMedia++
			}
		case "video":
			c.detail.OtherMediaTokens += metaUnknownVideoTokens
			c.detail.UnknownMedia++
		}
	}
}

type MetaImageTokenEstimate struct {
	Tokens          int64
	Width, Height   int
	KnownDimensions bool
}

// EstimateMetaImageSource reads at most 256 KiB of decoded header data and never fetches URLs.
func EstimateMetaImageSource(source map[string]any) MetaImageTokenEstimate {
	unknown := MetaImageTokenEstimate{Tokens: MetaUnknownImageTokens}
	if source["type"] != "base64" {
		return unknown
	}
	encoded, ok := source["data"].(string)
	if !ok {
		return unknown
	}
	header, err := io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)), metaImageHeaderBytes))
	if err != nil {
		return unknown
	}
	width, height := 0, 0
	if len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WEBP" {
		width, height = metaWebPDimensions(header)
	} else if configuration, format, err := image.DecodeConfig(bytes.NewReader(header)); err == nil && (format == "png" || format == "jpeg" || format == "gif") {
		width, height = configuration.Width, configuration.Height
	}
	if width <= 0 || height <= 0 || width > metaImageDimensionLimit || height > metaImageDimensionLimit || uint64(width)*uint64(height) > (1<<32)-1 {
		return unknown
	}
	// This gateway policy is inspired by standard 28-pixel vision patches, not Meta's tokenizer.
	scale := math.Min(1, 1568/math.Max(float64(width), float64(height)))
	w := math.Max(1, math.Floor(float64(width)*scale))
	h := math.Max(1, math.Floor(float64(height)*scale))
	patches := int64(math.Ceil(w/28) * math.Ceil(h/28))
	if patches > MetaUnknownImageTokens {
		patches = MetaUnknownImageTokens
	}
	return MetaImageTokenEstimate{Tokens: patches, Width: width, Height: height, KnownDimensions: true}
}

func metaWebPDimensions(header []byte) (int, int) {
	if len(header) < 12 {
		return 0, 0
	}
	declared := uint64(binary.LittleEndian.Uint32(header[4:8])) + 8
	available := uint64(len(header))
	if declared < available {
		available = declared
	}
	for offset := uint64(12); offset+8 <= available; {
		kind := string(header[offset : offset+4])
		size := uint64(binary.LittleEndian.Uint32(header[offset+4 : offset+8]))
		start := offset + 8
		if start+size > declared {
			return 0, 0
		}
		switch kind {
		case "VP8X":
			if size < 10 || start+10 > available {
				return 0, 0
			}
			p := header[start : start+10]
			return 1 + int(p[4]) + (int(p[5]) << 8) + (int(p[6]) << 16), 1 + int(p[7]) + (int(p[8]) << 8) + (int(p[9]) << 16)
		case "VP8L":
			if size < 5 || start+5 > available || header[start] != 0x2f {
				return 0, 0
			}
			bits := binary.LittleEndian.Uint32(header[start+1 : start+5])
			if bits>>29 != 0 {
				return 0, 0
			}
			return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1
		case "VP8 ":
			if size < 10 || start+10 > available {
				return 0, 0
			}
			p := header[start : start+10]
			if p[0]&1 != 0 || p[3] != 0x9d || p[4] != 1 || p[5] != 0x2a {
				return 0, 0
			}
			return int(binary.LittleEndian.Uint16(p[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(p[8:10]) & 0x3fff)
		}
		offset = start + size + (size & 1)
	}
	return 0, 0
}
