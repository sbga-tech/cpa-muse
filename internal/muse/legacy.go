package muse

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// legacyPlanner is the fork's default path: the host translates the client
// request to codex (OpenAI Responses as Codex sends it) and Meta answers in
// the same event format, which the host translates back.
//
// fromResponses marks native OpenAI Responses input in legacy mode. It only
// arrives when the plugin declares openai-response input for the profile
// engine; the plugin then performs the Responses-to-codex request conversion
// itself and emits Responses framing directly.
type legacyPlanner struct {
	fromResponses bool
}

// legacyDroppedFields are rejected or meaningless on api.meta.ai /v1/responses.
var legacyDroppedFields = []string{"generate", "prompt_cache_retention", "safety_identifier", "stream_options", "client_metadata"}

func (p legacyPlanner) plan(ctx context.Context, e *Executor, call *Call, cred Credential) (*responsesPlan, error) {
	body := bytes.Clone(call.Req.Payload)
	if len(bytes.TrimSpace(body)) == 0 || !gjson.ValidBytes(body) {
		return nil, errorf(http.StatusBadRequest, "invalid_json", "request body must be a JSON object")
	}
	plan := &responsesPlan{}
	if p.fromResponses {
		converted, err := convertResponsesRequestToCodex(call.Model, body)
		if err != nil {
			return nil, err
		}
		body = converted
		plan.sseFraming = true
		plan.nonStreamResponseObject = true
		original := call.Req.OriginalRequest
		if len(original) == 0 {
			original = call.Req.Payload
		}
		plan.transforms = append(plan.transforms, newCodexToResponsesTransformer(original))
	}
	body = setString(body, "model", call.Model)
	body = setBool(body, "stream", true)
	for _, field := range legacyDroppedFields {
		body, _ = sjson.DeleteBytes(body, field)
	}
	body = applyThinkingLevel(body, call.ThinkingLevel)

	bridge, patched, err := newApplyPatchBridge(call.Req.SourceFormat, call.Req.OriginalRequest, body)
	if err != nil {
		return nil, err
	}
	body = patched
	if bridge != nil {
		plan.transforms = append([]eventTransformer{bridge}, plan.transforms...)
	}

	if instructions := gjson.GetBytes(body, "instructions"); !instructions.Exists() || instructions.Type == gjson.Null {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}
	body = sanitizeMetaReasoning(body, cred.BaseURL)
	body = sanitizeMetaWebSearchTools(body)
	body = normalizeCodexToolIntegerTypes(body, call.Req.Headers)
	if e.cfg.InjectMuseNamespace() {
		body = ensureMuseToolNamespace(body)
	}
	session := sessionID(call.Req.Headers)
	body = ensurePromptCacheKey(body, session)

	identity := e.cfg.identity()
	extra := cred.Headers
	plan.body = body
	plan.headers = func(token string) http.Header {
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		h.Set("Authorization", "Bearer "+token)
		applyMuseIdentity(h, identity, session, call.Model)
		h.Set("Accept", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		for name, value := range extra {
			h.Set(name, value)
		}
		return h
	}
	return plan, nil
}

func (p legacyPlanner) count(ctx context.Context, e *Executor, call *Call) (pluginapi.ExecutorResponse, error) {
	return countResponsesLocal(call)
}

// applyThinkingLevel maps an unapplied "(level)" suffix to reasoning.effort.
func applyThinkingLevel(body []byte, level string) []byte {
	switch level {
	case "":
		return body
	case "none", "disabled", "off":
		out, _ := sjson.DeleteBytes(body, "reasoning.effort")
		return out
	}
	if _, err := strconv.Atoi(level); err == nil {
		return body
	}
	out, err := sjson.SetBytes(body, "reasoning.effort", level)
	if err != nil {
		return body
	}
	return out
}

// sanitizeMetaWebSearchTools drops search_content_types, which Meta rejects on
// web_search (it is accepted only on web_search_preview), including inside namespaces.
func sanitizeMetaWebSearchTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	var paths []string
	tools.ForEach(func(index, tool gjson.Result) bool {
		if tool.Get("type").String() == "web_search" && tool.Get("search_content_types").Exists() {
			paths = append(paths, fmt.Sprintf("tools.%d.search_content_types", index.Int()))
		}
		if tool.Get("type").String() == "namespace" {
			tool.Get("tools").ForEach(func(subIndex, sub gjson.Result) bool {
				if sub.Get("type").String() == "web_search" && sub.Get("search_content_types").Exists() {
					paths = append(paths, fmt.Sprintf("tools.%d.tools.%d.search_content_types", index.Int(), subIndex.Int()))
				}
				return true
			})
		}
		return true
	})
	for _, path := range paths {
		if out, err := sjson.DeleteBytes(body, path); err == nil {
			body = out
		}
	}
	return body
}

// museNamespace was captured from the Muse Code 1.4.12 reminder agent.
// Subscription-key tool requests can require it even with current client headers.
const museNamespace = `{"type":"namespace","name":"muse","description":"Muse Code tool set.","tools":[{"type":"function","name":"submit_reminder_decision","description":"Submit this reminder child's explicit decision.","parameters":{"additionalProperties":false,"properties":{"advisory_text":{"description":"Short text to deliver to the main agent when reminding.","type":["null","string"]},"confidence":{"description":"Confidence in the decision.","enum":["low","medium","high"],"type":["null","string"]},"decision":{"enum":["remind","none"],"type":"string"},"priority":{"description":"Requested reminder priority.","enum":["low","normal","high"],"type":["null","string"]},"reason":{"description":"Brief reason for reminding or staying silent.","type":"string"},"skill_id":{"description":"The listed skill id to remind when needs_reminder is true.","type":["null","string"]},"visible_for_steps":{"description":"Requested ephemeral visibility length in main model steps.","maximum":8,"minimum":1,"type":["null","integer"]}},"required":["advisory_text","confidence","decision","priority","reason","skill_id","visible_for_steps"],"type":"object"},"strict":true}]}`

// ensureMuseToolNamespace appends the canonical namespace to tool-bearing
// requests, keeping caller tool indexes. Tool-less requests and requests that
// already carry a muse namespace are untouched.
func ensureMuseToolNamespace(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	items := tools.Array()
	if len(items) == 0 {
		return body
	}
	for _, item := range items {
		if item.Get("type").String() == "namespace" && item.Get("name").String() == "muse" {
			return body
		}
	}
	raw := make([]byte, 0, len(tools.Raw)+len(museNamespace)+2)
	raw = append(raw, '[')
	for i, item := range items {
		if i > 0 {
			raw = append(raw, ',')
		}
		raw = append(raw, item.Raw...)
	}
	raw = append(raw, ',')
	raw = append(raw, museNamespace...)
	raw = append(raw, ']')
	out, err := sjson.SetRawBytes(body, "tools", raw)
	if err != nil {
		return body
	}
	return out
}

// collectOutputItemDone records a finished output item by its output_index.
func collectOutputItemDone(event []byte, byIndex map[int64][]byte, fallback *[][]byte) {
	item := gjson.GetBytes(event, "item")
	if !item.Exists() || item.Type != gjson.JSON {
		return
	}
	if index := gjson.GetBytes(event, "output_index"); index.Exists() {
		byIndex[index.Int()] = []byte(item.Raw)
		return
	}
	*fallback = append(*fallback, []byte(item.Raw))
}

// patchCompletedOutput fills an empty response.output from streamed output
// items, or restores missing item ids on a populated one, as the fork does.
func patchCompletedOutput(event []byte, byIndex map[int64][]byte, fallback [][]byte) []byte {
	output := gjson.GetBytes(event, "response.output")
	if output.IsArray() && len(output.Array()) > 0 {
		return hydrateOutputItemIDs(event, output.Array(), byIndex)
	}
	if len(byIndex) == 0 && len(fallback) == 0 {
		return event
	}
	indexes := make([]int64, 0, len(byIndex))
	for idx := range byIndex {
		indexes = append(indexes, idx)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	var buf bytes.Buffer
	buf.WriteByte('[')
	n := 0
	for _, idx := range indexes {
		if n > 0 {
			buf.WriteByte(',')
		}
		buf.Write(byIndex[idx])
		n++
	}
	for _, item := range fallback {
		if n > 0 {
			buf.WriteByte(',')
		}
		buf.Write(item)
		n++
	}
	buf.WriteByte(']')
	out, err := sjson.SetRawBytes(event, "response.output", buf.Bytes())
	if err != nil {
		return event
	}
	return out
}

func hydrateOutputItemIDs(event []byte, items []gjson.Result, byIndex map[int64][]byte) []byte {
	patched := event
	for index, item := range items {
		if id := item.Get("id"); id.Exists() && id.Type != gjson.Null && (id.Type != gjson.String || strings.TrimSpace(id.String()) != "") {
			continue
		}
		streamed, ok := byIndex[int64(index)]
		if !ok {
			continue
		}
		id := gjson.GetBytes(streamed, "id")
		if id.Type != gjson.String || strings.TrimSpace(id.String()) == "" {
			continue
		}
		if out, err := sjson.SetRawBytes(patched, "response.output."+strconv.Itoa(index)+".id", []byte(id.Raw)); err == nil {
			patched = out
		}
	}
	return patched
}

func setString(body []byte, path, value string) []byte {
	if gjson.GetBytes(body, path).String() == value && gjson.GetBytes(body, path).Type == gjson.String {
		return body
	}
	out, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return body
	}
	return out
}

func setBool(body []byte, path string, value bool) []byte {
	current := gjson.GetBytes(body, path)
	if current.Exists() && current.IsBool() && current.Bool() == value {
		return body
	}
	out, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return body
	}
	return out
}
