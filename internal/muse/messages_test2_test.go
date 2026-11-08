package muse

// Consumer-visible regression cases ported from fork helps/meta_messages_test2_test.go.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"reflect"
	"strings"
	"testing"
)

func TestMetaMessagesContextManagementAcceptedUnchanged(t *testing.T) {
	for _, count := range []bool{false, true} {
		root, err := DecodeMetaMessages([]byte(`{"model":"muse-spark-1.3","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"context_management":{"edits":[{"type":"clear_tool_uses_20250919","keep":{"type":"tool_uses","value":3},"cache_control":{"ttl":"1h"}}]},"cache_control":{"type":"ephemeral","ttl":"1h"}}`))
		if err != nil {
			t.Fatal(err)
		}
		original, _ := json.Marshal(root["context_management"])
		policy := MetaMessagesPolicy{Degrade: MessagesDegradeConfig{CacheTTL5m: true}}
		if err := NormalizeMetaMessages(root, "muse-spark-1.3", false, count, policy); err != nil {
			t.Fatal(err)
		}
		if err := ValidateMetaMessages(root, count, policy); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(root["context_management"])
		if !bytes.Equal(original, after) {
			t.Fatal("opaque context management was altered")
		}
		policy.Degrade.ContextManagementDrop = true
		if err := NormalizeMetaMessages(root, "muse-spark-1.3", false, count, policy); err != nil {
			t.Fatal(err)
		}
		if _, exists := root["context_management"]; exists {
			t.Fatal("explicit drop option was ignored")
		}
	}
}

func TestMetaMessagesReportedRejectionsAndInvalidEffort(t *testing.T) {
	for _, tt := range []struct{ extra, model, code string }{
		{`,"tools":[{"type":"code_execution_20250825"}]`, "muse-spark-1.3", "unsupported"},
		{`,"tools":[{"type":"tool_search_tool_regex_20251119"}]`, "muse-spark-1.3", "unsupported"},
		{`,"tools":[{"type":"tool_search_tool_bm25_20251119"}]`, "muse-spark-1.3", "capability_unverified"},
		{`,"output_config":{"effort":"max"}`, "muse-spark-1.2", "unsupported"},
		{`,"output_config":{"effort":"ultra"}`, "muse-spark-1.3", "invalid_value"},
		{`,"output_config":{"effort":""}`, "muse-spark-1.3", "invalid_value"},
		{`,"output_config":{"effort":3}`, "muse-spark-1.3", "invalid_value"},
		{`,"context_management":null`, "muse-spark-1.3", "invalid_value"},
	} {
		body := []byte(`{"model":"` + tt.model + `","max_tokens":100,"messages":[{"role":"user","content":"hello"}]` + tt.extra + `}`)
		root, err := DecodeMetaMessages(body)
		if err != nil {
			t.Fatal(err)
		}
		err = ValidateMetaMessages(root, false, MetaMessagesPolicy{Capabilities: []string{"effort.max", "hosted-tool-search-o1"}})
		var response *StatusError
		if !errors.As(err, &response) || !bytes.Contains(response.Body, []byte("[cpa.meta.messages."+tt.code+"]")) {
			t.Fatalf("expected %s for %s; got %v", tt.code, body, err)
		}
	}
}

func TestMetaMessagesNativeToolSearchModelGatesAndReplay(t *testing.T) {
	for _, model := range []string{"muse-wonder-full-1.0", "muse-wonder-1.0", "muse-spark-1.2", "muse-spark-1.3-contributor", "muse-spark-1.3"} {
		t.Run(model, func(t *testing.T) {
			root, err := DecodeMetaMessages([]byte(`{"model":"` + model + `","max_tokens":2048,"tools":[{"type":"tool_search"},{"name":"read_file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}},"defer_loading":true}],"messages":[{"role":"user","content":"find a tool"},{"role":"assistant","content":[{"type":"server_tool_use","id":"call_search","name":"tool_search","input":{"paths":["read_file"]}},{"type":"tool_search_tool_result","tool_use_id":"call_search","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"read_file"}]}}]},{"role":"user","content":"continue"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(root)
			policy := MetaMessagesPolicy{}
			if model != "muse-spark-1.3" {
				policy.Capabilities = []string{"hosted-tool-search-o1"}
			}
			err = ValidateMetaMessages(root, false, policy)
			if (err == nil) != (model != "muse-spark-1.3") {
				t.Fatalf("model gate: %v", err)
			}
			after, _ := json.Marshal(root)
			if !bytes.Equal(before, after) {
				t.Fatal("O1 validation rewrote tool history")
			}
		})
	}
}

func metaTestPNGHeader(width, height uint32) []byte {
	result := []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 'I', 'H', 'D', 'R'}
	data := make([]byte, 13)
	binary.BigEndian.PutUint32(data[:4], width)
	binary.BigEndian.PutUint32(data[4:8], height)
	data[8] = 8
	data[9] = 6
	result = append(result, data...)
	checksum := make([]byte, 4)
	binary.BigEndian.PutUint32(checksum, crc32.ChecksumIEEE(result[12:]))
	return append(result, checksum...)
}

func metaTestImageSource(data []byte) map[string]any {
	return map[string]any{"type": "base64", "data": base64.StdEncoding.EncodeToString(data)}
}

func TestMetaMessagesImageDimensionEstimateSmallMediumLarge(t *testing.T) {
	var previous int64
	for _, size := range []uint32{16, 512, 4096} {
		estimate := EstimateMetaImageSource(metaTestImageSource(metaTestPNGHeader(size, size)))
		if !estimate.KnownDimensions || estimate.Width != int(size) || estimate.Height != int(size) || estimate.Tokens <= previous || estimate.Tokens > 1568 {
			t.Fatalf("size=%d estimate=%+v", size, estimate)
		}
		if size == 16 && estimate.Tokens != 1 {
			t.Fatalf("tiny image charged %d tokens", estimate.Tokens)
		}
		body := metaTestCountBody([]any{map[string]any{"type": "image", "source": metaTestImageSource(metaTestPNGHeader(size, size))}})
		detail, err := EstimateMetaMessagesTokenDetails(body)
		if err != nil {
			t.Fatal(err)
		}
		if detail.FramingTokens != 12 || detail.TextTokens != 0 || detail.ImageTokens != estimate.Tokens || detail.InputTokens != estimate.Tokens+12 {
			t.Fatalf("unexpected fixture breakdown: %+v", detail)
		}
		t.Logf("%dx%d: text=%d image=%d framing=%d total=%d unknown_media=%d", size, size, detail.TextTokens, detail.ImageTokens, detail.FramingTokens, detail.InputTokens, detail.UnknownMedia)
		previous = estimate.Tokens
	}
}

func TestMetaMessagesImageFormatsReadHeadersOnly(t *testing.T) {
	im := image.NewRGBA(image.Rect(0, 0, 33, 57))
	for _, format := range []string{"png", "jpeg", "gif"} {
		var encoded bytes.Buffer
		var err error
		switch format {
		case "png":
			err = png.Encode(&encoded, im)
		case "jpeg":
			err = jpeg.Encode(&encoded, im, nil)
		case "gif":
			err = gif.Encode(&encoded, im, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		estimate := EstimateMetaImageSource(metaTestImageSource(encoded.Bytes()))
		if !estimate.KnownDimensions || estimate.Width != 33 || estimate.Height != 57 {
			t.Fatalf("%s: %+v", format, estimate)
		}
	}
}

func metaTestWebPChunk(kind string, payload []byte) []byte {
	header := []byte("RIFF\x00\x00\x00\x00WEBP" + kind + "\x00\x00\x00\x00")
	binary.LittleEndian.PutUint32(header[16:20], uint32(len(payload)))
	header = append(header, payload...)
	if len(payload)%2 != 0 {
		header = append(header, 0)
	}
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(header)-8))
	return header
}

func TestMetaMessagesWebPHeaderVariantsAndMaliciousDimensions(t *testing.T) {
	vp8 := []byte{0, 0, 0, 0x9d, 1, 0x2a, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(vp8[6:8], 320)
	binary.LittleEndian.PutUint16(vp8[8:10], 240)
	vp8l := []byte{0x2f, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(vp8l[1:], uint32(319|(239<<14)))
	vp8x := []byte{0, 0, 0, 0, 0x3f, 1, 0, 0xef, 0, 0}
	for kind, payload := range map[string][]byte{"VP8 ": vp8, "VP8L": vp8l, "VP8X": vp8x} {
		estimate := EstimateMetaImageSource(metaTestImageSource(metaTestWebPChunk(kind, payload)))
		if !estimate.KnownDimensions || estimate.Width != 320 || estimate.Height != 240 {
			t.Fatalf("%s: %+v", kind, estimate)
		}
	}
	malicious := metaTestWebPChunk("VP8X", []byte{0, 0, 0, 0, 255, 255, 255, 255, 255, 255})
	badChunk := metaTestWebPChunk("VP8X", vp8x)
	binary.LittleEndian.PutUint32(badChunk[16:20], ^uint32(0))
	for _, encoded := range [][]byte{malicious, badChunk, metaTestPNGHeader(^uint32(0), ^uint32(0)), metaTestPNGHeader(0, 1), metaTestPNGHeader(1<<20, 1<<20)} {
		estimate := EstimateMetaImageSource(metaTestImageSource(encoded))
		if estimate.KnownDimensions || estimate.Tokens != MetaUnknownImageTokens {
			t.Fatalf("malicious dimensions were accepted: %+v", estimate)
		}
	}
	largePrefix := metaTestWebPChunk("JUNK", make([]byte, metaImageHeaderBytes+1))
	largePrefix = append(largePrefix, metaTestWebPChunk("VP8X", vp8x)[12:]...)
	binary.LittleEndian.PutUint32(largePrefix[4:8], uint32(len(largePrefix)-8))
	if estimate := EstimateMetaImageSource(metaTestImageSource(largePrefix)); estimate.KnownDimensions {
		t.Fatal("image inspection exceeded its header budget")
	}
}

func metaTestCountBody(content any) []byte {
	body, _ := json.Marshal(map[string]any{"model": "muse-test", "messages": []any{map[string]any{"role": "user", "content": content}}})
	return body
}

func TestMetaMessagesEstimateDoesNotCountEncodedBinary(t *testing.T) {
	blank := image.NewRGBA(image.Rect(0, 0, 512, 512))
	noisy := image.NewRGBA(blank.Bounds())
	state := uint32(123456789)
	for i := range noisy.Pix {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		noisy.Pix[i] = byte(state)
	}
	var compact, large bytes.Buffer
	if err := png.Encode(&compact, blank); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&large, noisy); err != nil {
		t.Fatal(err)
	}
	if large.Len() < compact.Len()*10 {
		t.Fatal("fixtures do not exercise encoded-size inflation")
	}
	var counts []MetaMessagesTokenEstimate
	for _, data := range [][]byte{compact.Bytes(), large.Bytes()} {
		body := metaTestCountBody([]any{map[string]any{"type": "image", "source": metaTestImageSource(data)}})
		before := bytes.Clone(body)
		detail, err := EstimateMetaMessagesTokenDetails(body)
		if err != nil {
			t.Fatal(err)
		}
		counts = append(counts, detail)
		if !bytes.Equal(body, before) {
			t.Fatal("estimation modified the send payload")
		}
	}
	if !reflect.DeepEqual(counts[0], counts[1]) || counts[0].ImageTokens != 361 || counts[0].TextTokens != 0 {
		t.Fatalf("binary size changed estimate: %+v", counts)
	}
}

func TestMetaMessagesSemanticEstimateTextToolsAndUnknownMedia(t *testing.T) {
	plain, err := EstimateMetaMessagesTokenDetails(metaTestCountBody("Hello there"))
	if err != nil {
		t.Fatal(err)
	}
	if plain.FramingTokens != 12 || plain.InputTokens >= 30 {
		t.Fatalf("JSON transport inflated simple text: %+v", plain)
	}
	t.Logf("plain text: text=%d framing=%d total=%d", plain.TextTokens, plain.FramingTokens, plain.InputTokens)
	long, err := EstimateMetaMessagesTokenDetails(metaTestCountBody(strings.Repeat("ordinary prompt text ", 5000)))
	if err != nil || long.InputTokens <= plain.InputTokens+5000 {
		t.Fatalf("text omitted: %+v %v", long, err)
	}
	root := map[string]any{"model": "muse-test", "messages": []any{
		map[string]any{"role": "user", "content": "run tool"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call_1", "name": "write_file", "input": map[string]any{"content": strings.Repeat("large tool argument ", 20000)}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": []any{map[string]any{"type": "image", "source": metaTestImageSource(metaTestPNGHeader(16, 16))}}}}},
	}}
	body, _ := json.Marshal(root)
	history, err := EstimateMetaMessagesTokenDetails(body)
	if err != nil || history.TextTokens < 20000 || history.ImageTokens != 1 {
		t.Fatalf("large arguments or tool-result image omitted: %+v %v", history, err)
	}
	remote, err := EstimateMetaMessagesTokenDetails(metaTestCountBody([]any{map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "http://127.0.0.1:1/must-not-be-fetched"}}}))
	if err != nil || remote.UnknownMedia != 1 || remote.ImageTokens != MetaUnknownImageTokens || remote.Uncertainty() != "uncalibrated,unknown-media" {
		t.Fatalf("remote fallback: %+v %v", remote, err)
	}
	if _, err := EstimateMetaMessagesTokenDetails(metaTestCountBody([]any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "not-base64"}}})); err != nil {
		t.Fatalf("unknown image should yield an explicit heuristic fallback: %v", err)
	}
}
