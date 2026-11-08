package muse

// Ported from the fork internal/runtime/executor/helps/meta_messages_transport.go.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// MetaMessagesMetering keeps missing provider values distinct from measured zero.
type MetaMessagesMetering struct {
	InputTokens                                                     *int64
	OutputTokens                                                    *int64
	CacheReadInputTokens                                            *int64
	CacheCreationInputTokens                                        *int64
	inputSource, outputSource, cacheReadSource, cacheCreationSource string
	final                                                           bool
}

func (m *MetaMessagesMetering) Observe(data []byte, final bool) {
	if m == nil || !gjson.ValidBytes(data) {
		return
	}
	paths := []string{"usage", "message.usage"}
	if gjson.GetBytes(data, "type").String() == "message_start" {
		paths[0], paths[1] = paths[1], paths[0]
	}
	validOutput := false
	for _, counter := range []struct {
		key    string
		target **int64
		source *string
	}{
		{"input_tokens", &m.InputTokens, &m.inputSource},
		{"output_tokens", &m.OutputTokens, &m.outputSource},
		{"cache_read_input_tokens", &m.CacheReadInputTokens, &m.cacheReadSource},
		{"cache_creation_input_tokens", &m.CacheCreationInputTokens, &m.cacheCreationSource},
	} {
		for _, path := range paths {
			value := gjson.GetBytes(data, path+"."+counter.key)
			if value.Type != gjson.Number {
				continue
			}
			number, err := strconv.ParseInt(value.Raw, 10, 64)
			if err != nil || number < 0 {
				continue
			}
			*counter.target = &number
			*counter.source = path + "." + counter.key
			if counter.key == "output_tokens" {
				validOutput = true
			}
			break
		}
	}
	if final && validOutput {
		m.final = true
	}
}

// MetaMessagesBetaHeader preserves all validated beta values, including noncanonical SDK map keys.
func MetaMessagesBetaHeader(headers http.Header) string {
	var values []string
	for key, entries := range headers {
		if strings.EqualFold(key, "Anthropic-Beta") {
			values = append(values, entries...)
		}
	}
	return strings.Join(values, ",")
}

func (m *MetaMessagesMetering) Status() string {
	if m.InputTokens == nil && m.OutputTokens == nil && m.CacheReadInputTokens == nil && m.CacheCreationInputTokens == nil {
		return "unknown"
	}
	if m.final && m.InputTokens != nil && m.OutputTokens != nil {
		return "complete"
	}
	return "partial"
}

func (m *MetaMessagesMetering) logFields() map[string]any {
	fields := map[string]any{"metering": m.Status()}
	for _, counter := range []struct {
		key, sourceKey, source string
		value                  *int64
	}{
		{"input_tokens", "input_usage_source", m.inputSource, m.InputTokens},
		{"output_tokens", "output_usage_source", m.outputSource, m.OutputTokens},
		{"cache_read_input_tokens", "cache_read_usage_source", m.cacheReadSource, m.CacheReadInputTokens},
		{"cache_creation_input_tokens", "cache_creation_usage_source", m.cacheCreationSource, m.CacheCreationInputTokens},
	} {
		fields[counter.key] = nil
		if counter.value != nil {
			fields[counter.key] = *counter.value
		}
		if counter.source != "" {
			fields[counter.sourceKey] = counter.source
		}
	}
	return fields
}

// ApplyMetaMessagesHeaders constructs a fresh header set. Auth attributes and client credentials never pass through.
func ApplyMetaMessagesHeaders(req *http.Request, token string, stream bool, inbound http.Header) {
	req.Header = make(http.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "CLIProxyAPI-Meta-Messages/1.0")
	version := strings.TrimSpace(inbound.Get("Anthropic-Version"))
	if version == "" {
		version = "2023-06-01"
	}
	req.Header.Set("Anthropic-Version", version)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
}

// MetaSSEReader buffers one bounded SSE event and supports multiline data and CRLF.
type MetaSSEReader struct {
	reader *bufio.Reader
	limit  int
}

func NewMetaSSEReader(body io.Reader) *MetaSSEReader {
	return &MetaSSEReader{reader: bufio.NewReader(body), limit: 16 << 20}
}

func (r *MetaSSEReader) Next() ([]byte, []byte, error) {
	var frame, data, line []byte
	for {
		part, err := r.reader.ReadSlice('\n')
		if len(frame)+len(line)+len(part) > r.limit {
			return nil, nil, fmt.Errorf("Meta Messages SSE event exceeds 16 MiB")
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(frame) == 0 && len(line) == 0 {
				return nil, nil, io.EOF
			}
			if err == io.EOF {
				return nil, nil, io.ErrUnexpectedEOF
			}
			return nil, nil, err
		}
		frame = append(frame, line...)
		content := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(content) == 0 {
			return frame, bytes.TrimSuffix(data, []byte("\n")), nil
		}
		if bytes.HasPrefix(content, []byte("data:")) {
			value := bytes.TrimPrefix(content, []byte("data:"))
			value = bytes.TrimPrefix(value, []byte(" "))
			data = append(data, value...)
			data = append(data, '\n')
		}
		line = line[:0]
	}
}

// MetaMessagesDownstreamHeaders retains only response metadata relevant to clients.
func MetaMessagesDownstreamHeaders(in http.Header) http.Header {
	out := http.Header{}
	for _, name := range []string{"Content-Type", "Request-Id", "X-Request-Id", "Retry-After", "Anthropic-Ratelimit-Requests-Limit", "Anthropic-Ratelimit-Requests-Remaining", "Anthropic-Ratelimit-Requests-Reset", "Anthropic-Ratelimit-Tokens-Limit", "Anthropic-Ratelimit-Tokens-Remaining", "Anthropic-Ratelimit-Tokens-Reset"} {
		if values := in.Values(name); len(values) > 0 {
			out[name] = append([]string(nil), values...)
		}
	}
	return out
}
