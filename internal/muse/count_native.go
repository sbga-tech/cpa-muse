package muse

import (
	"encoding/json"
	"math"
	"strings"
	"sync"

	"github.com/tiktoken-go/tokenizer"
)

var inputTokenizerOnce sync.Once
var inputTokenizerCodec tokenizer.Codec
var inputTokenizerErr error

// claudeInputTokenizer ports helps/claude_input_tokens.go's shared O200k codec.
func claudeInputTokenizer() (tokenizer.Codec, error) {
	inputTokenizerOnce.Do(func() {
		inputTokenizerCodec, inputTokenizerErr = tokenizer.Get(tokenizer.O200kBase)
	})
	return inputTokenizerCodec, inputTokenizerErr
}

// estimateNativeResponses ports helps/meta_responses_native.go. Media allowances
// and the 15% margin are heuristic, not provider-verified capacity bounds.
func estimateNativeResponses(body []byte) (int64, error) {
	root, err := DecodeMetaMessages(body)
	if err != nil {
		return 0, err
	}
	semantic := make(map[string]any)
	for _, key := range []string{"input", "instructions", "tools", "tool_choice", "text", "reasoning"} {
		if value, exists := root[key]; exists {
			semantic[key] = value
		}
	}
	media := estimateMetaNativeMedia(semantic["input"])
	encoded, err := json.Marshal(semantic)
	if err != nil {
		return 0, err
	}
	codec, err := claudeInputTokenizer()
	if err != nil {
		return 0, err
	}
	count, err := codec.Count(string(encoded))
	if err != nil {
		return 0, err
	}
	return int64(math.Ceil(float64(int64(count)+media+32) * 1.15)), nil
}

func estimateMetaNativeMedia(value any) int64 {
	var count int64
	switch v := value.(type) {
	case map[string]any:
		kind, _ := v["type"].(string)
		switch kind {
		case "input_image":
			source := map[string]any{"type": "url"}
			if data, ok := v["image_url"].(string); ok && strings.HasPrefix(data, "data:") {
				if header, encoded, found := strings.Cut(data, ","); found && strings.HasSuffix(header, ";base64") {
					source = map[string]any{"type": "base64", "data": encoded}
				}
			}
			count += EstimateMetaImageSource(source).Tokens
		case "input_file":
			count += 32768
		case "input_video":
			count += 65536
		}
		if kind == "input_image" || kind == "input_file" || kind == "input_video" {
			for _, key := range []string{"image_url", "file_data", "video_url"} {
				if data, ok := v[key].(string); ok && (key == "file_data" || strings.HasPrefix(data, "data:")) {
					v[key] = ""
				}
			}
		}
		for key, child := range v {
			if key != "arguments" {
				count += estimateMetaNativeMedia(child)
			}
		}
	case []any:
		for _, child := range v {
			count += estimateMetaNativeMedia(child)
		}
	}
	return count
}
