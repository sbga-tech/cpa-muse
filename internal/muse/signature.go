package muse

import (
	"net/url"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// sanitizeMetaReasoning ports the Meta-target path of fork
// internal/runtime/executor/openai_responses_signature.go. Only recognized Meta
// envelopes replay: native Meta rejects router suffixes, OpenRouter preserves
// them, and other routers receive the native envelope without the suffix.
// Request-scoped debug logs are omitted because this pure helper has no host
// callback or request context. Provider inference is unnecessary for a fixed
// Meta target; only the inspector's alphabet helper is needed from the fork's
// internal/signature/provider_compatibility.go.
func sanitizeMetaReasoning(body []byte, baseURL string) []byte {
	if len(body) == 0 {
		return body
	}
	// Ported from fork internal/util/gjson.go. The result aliases body, which
	// remains immutable; all sjson edits below allocate independent output.
	input := gjson.Get(unsafe.String(unsafe.SliceData(body), len(body)), "input")
	if !input.IsArray() {
		return body
	}
	nativeOnly := isNativeMetaUpstreamURL(baseURL)
	allowRouterSuffix := isOpenRouterUpstreamURL(baseURL)
	stripOrphanIDs := !gjson.GetBytes(body, "store").Bool()
	items := input.Array()
	var rebuilt []byte
	written := 0
	keep := func(raw string) {
		if rebuilt == nil {
			return
		}
		if written > 0 {
			rebuilt = append(rebuilt, ',')
		}
		rebuilt = append(rebuilt, raw...)
		written++
	}
	startRebuild := func(index int) {
		if rebuilt != nil {
			return
		}
		rebuilt = make([]byte, 0, len(input.Raw))
		rebuilt = append(rebuilt, '[')
		for i := range index {
			keep(items[i].Raw)
		}
	}
	for index, item := range items {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			keep(item.Raw)
			continue
		}
		nextItem := item.Raw
		changed := false
		content := item.Get("content")
		if content.IsArray() && len(content.Array()) > 0 {
			if openaiResponsesReasoningSummaryIsEmpty(item.Get("summary")) {
				if promoted, err := promoteOpenAIResponsesReasoningTextToSummary(nextItem, content); err == nil {
					nextItem = promoted
				}
			}
			if cleared, err := sjson.SetRaw(nextItem, "content", "[]"); err == nil {
				nextItem, changed = cleared, true
			}
		}
		encrypted := item.Get("encrypted_content")
		valid := false
		rewritten := ""
		if encrypted.Type == gjson.String {
			raw := encrypted.String()
			if raw == strings.TrimSpace(raw) {
				if info, err := InspectMetaReasoningSignature(raw); err == nil {
					valid = info.RouterSuffix == "" || !nativeOnly
					if valid && info.RouterSuffix != "" && !allowRouterSuffix {
						rewritten = stripRouterSuffix(raw, info.RouterSuffix)
					}
				}
			}
		}
		if encrypted.Exists() {
			if valid {
				if rewritten != "" {
					if updated, err := sjson.Set(nextItem, "encrypted_content", rewritten); err == nil {
						nextItem, changed = updated, true
					}
				}
			} else if dropped, err := sjson.Delete(nextItem, "encrypted_content"); err == nil {
				nextItem, changed = dropped, true
				if stripOrphanIDs && item.Get("id").Exists() {
					if droppedID, err := sjson.Delete(nextItem, "id"); err == nil {
						nextItem = droppedID
					}
				}
			}
		} else if stripOrphanIDs && item.Get("id").Exists() {
			if dropped, err := sjson.Delete(nextItem, "id"); err == nil {
				nextItem, changed = dropped, true
			}
		}
		if changed {
			startRebuild(index)
			keep(nextItem)
		} else {
			keep(item.Raw)
		}
	}
	if rebuilt == nil {
		return body
	}
	rebuilt = append(rebuilt, ']')
	if updated, err := sjson.SetRawBytes(body, "input", rebuilt); err == nil {
		return updated
	}
	return body
}

func openaiResponsesReasoningSummaryIsEmpty(summary gjson.Result) bool {
	if !summary.Exists() || summary.Type == gjson.Null {
		return true
	}
	return summary.IsArray() && len(summary.Array()) == 0
}

func promoteOpenAIResponsesReasoningTextToSummary(itemRaw string, content gjson.Result) (string, error) {
	var b strings.Builder
	b.WriteByte('[')
	n := 0
	for _, part := range content.Array() {
		if strings.TrimSpace(part.Get("type").String()) != "reasoning_text" {
			continue
		}
		text := part.Get("text").String()
		if text == "" {
			continue
		}
		partJSON, err := sjson.Set(`{"type":"summary_text"}`, "text", text)
		if err != nil {
			return itemRaw, err
		}
		if n > 0 {
			b.WriteByte(',')
		}
		b.WriteString(partJSON)
		n++
	}
	b.WriteByte(']')
	if n == 0 {
		return itemRaw, nil
	}
	return sjson.SetRaw(itemRaw, "summary", b.String())
}

func isNativeMetaUpstreamURL(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return true
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "" || host == "api.meta.ai" || strings.HasSuffix(host, ".meta.ai")
}

func isOpenRouterUpstreamURL(rawURL string) bool {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai")
}

func stripRouterSuffix(rawSignature, suffix string) string {
	if suffix == "" || len(rawSignature) <= len(suffix)+1 {
		return rawSignature
	}
	return rawSignature[:len(rawSignature)-len(suffix)-1]
}
