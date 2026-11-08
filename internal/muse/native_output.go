package muse

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// MetaResponsesOutput preserves emitted snapshots when a native terminal response omits them.
// It retains reasoning summary deltas, not the complete SSE transcript or ordinary text deltas.
type MetaResponsesOutput struct {
	items   []*metaObservedOutputItem
	byIndex map[int64]*metaObservedOutputItem
	byID    map[string]*metaObservedOutputItem
}

type metaObservedOutputItem struct {
	raw     []byte
	index   int64
	indexed bool
	summary map[int64]*metaObservedSummaryPart
}

type metaObservedSummaryPart struct {
	raw  []byte
	text *strings.Builder
}

// Transform changes only a completed/incomplete event's output array, using actual observations.
// It never creates a terminal event, marks an item done, or derives content from usage counters.
func (s *MetaResponsesOutput) Transform(event []byte) []byte {
	if s == nil || !gjson.ValidBytes(event) {
		return event
	}
	root := gjson.ParseBytes(event)
	switch root.Get("type").String() {
	case "response.output_item.added", "response.output_item.done":
		s.observeItem(root)
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		s.observeSummary(root)
	case "response.completed", "response.incomplete":
		return s.patchTerminal(event)
	}
	return event
}

// StreamLine preserves the source's SSE prefix and delimiters, including bridge-produced frames.
func (s *MetaResponsesOutput) StreamLine(line []byte) []byte {
	if s == nil || !bytes.HasPrefix(line, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(line[5:])
	updated := s.Transform(payload)
	if bytes.Equal(updated, payload) {
		return line
	}
	start := bytes.Index(line[5:], payload) + 5
	result := make([]byte, 0, len(line)-len(payload)+len(updated))
	result = append(result, line[:start]...)
	result = append(result, updated...)
	return append(result, line[start+len(payload):]...)
}

func metaOutputIndex(value gjson.Result) (int64, bool) {
	if value.Type != gjson.Number {
		return 0, false
	}
	index, err := strconv.ParseInt(value.Raw, 10, 64)
	return index, err == nil && index >= 0
}

func (s *MetaResponsesOutput) observeItem(event gjson.Result) {
	item := event.Get("item")
	if !item.IsObject() {
		return
	}
	if s.byIndex == nil {
		s.byIndex = map[int64]*metaObservedOutputItem{}
		s.byID = map[string]*metaObservedOutputItem{}
	}
	index, indexed := metaOutputIndex(event.Get("output_index"))
	id := item.Get("id").String()
	var previous *metaObservedOutputItem
	if indexed {
		previous = s.byIndex[index]
	}
	if previous == nil && id != "" {
		previous = s.byID[id]
	}
	if previous == nil {
		previous = &metaObservedOutputItem{}
		s.items = append(s.items, previous)
	}
	oldID := gjson.GetBytes(previous.raw, "id").String()
	oldType := gjson.GetBytes(previous.raw, "type").String()
	if (oldID != "" && id != "" && oldID != id) || (oldType != "" && item.Get("type").String() != "" && oldType != item.Get("type").String()) {
		delete(s.byID, oldID)
		previous.raw, previous.summary = nil, nil
	}
	previous.raw = mergeMetaOutputSnapshot(previous.snapshot(), []byte(item.Raw))
	previous.summary = nil
	if indexed {
		if previous.indexed && previous.index != index {
			delete(s.byIndex, previous.index)
		}
		previous.index, previous.indexed = index, true
		s.byIndex[index] = previous
	}
	if id != "" {
		s.byID[id] = previous
	}
}

func (s *MetaResponsesOutput) itemForEvent(event gjson.Result) *metaObservedOutputItem {
	if id := event.Get("item_id").String(); id != "" {
		if item := s.byID[id]; item != nil {
			return item
		}
	}
	if index, ok := metaOutputIndex(event.Get("output_index")); ok {
		item := s.byIndex[index]
		if item != nil {
			id, knownID := event.Get("item_id").String(), gjson.GetBytes(item.raw, "id").String()
			if id == "" || knownID == "" || id == knownID {
				return item
			}
		}
	}
	return nil
}

func (s *MetaResponsesOutput) observeSummary(event gjson.Result) {
	item := s.itemForEvent(event)
	index, ok := metaOutputIndex(event.Get("summary_index"))
	if item == nil || !ok || gjson.GetBytes(item.raw, "type").String() != "reasoning" {
		return
	}
	if item.summary == nil {
		item.summary = map[int64]*metaObservedSummaryPart{}
		for i, part := range gjson.GetBytes(item.raw, "summary").Array() {
			item.summary[int64(i)] = &metaObservedSummaryPart{raw: []byte(part.Raw)}
		}
	}
	part := item.summary[index]
	if part == nil {
		part = &metaObservedSummaryPart{}
		item.summary[index] = part
	}
	switch event.Get("type").String() {
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		if snapshot := event.Get("part"); snapshot.IsObject() {
			part.raw = mergeMetaOutputSnapshot(part.snapshot(), []byte(snapshot.Raw))
			part.text = nil
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		field := "delta"
		if strings.HasSuffix(event.Get("type").String(), ".done") {
			field = "text"
		}
		text := event.Get(field)
		if text.Type != gjson.String {
			return
		}
		if len(part.raw) == 0 {
			part.raw = []byte(`{"type":"summary_text","text":""}`)
		}
		if field == "text" {
			part.raw, _ = sjson.SetBytes(part.raw, "text", text.String())
			part.text = nil
			return
		}
		if part.text == nil {
			part.text = &strings.Builder{}
			part.text.WriteString(gjson.GetBytes(part.raw, "text").String())
		}
		part.text.WriteString(text.String())
	}
}

func (p *metaObservedSummaryPart) snapshot() []byte {
	if p.text == nil {
		return p.raw
	}
	updated, err := sjson.SetBytes(p.raw, "text", p.text.String())
	if err != nil {
		return p.raw
	}
	return updated
}

func (item *metaObservedOutputItem) snapshot() []byte {
	if len(item.summary) == 0 {
		return item.raw
	}
	indexes := make([]int64, 0, len(item.summary))
	for index := range item.summary {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	parts := make([][]byte, 0, len(indexes))
	for _, index := range indexes {
		if part := item.summary[index].snapshot(); len(part) > 0 {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return item.raw
	}
	updated, err := sjson.SetRawBytes(item.raw, "summary", joinMetaOutputItems(parts))
	if err != nil {
		return item.raw
	}
	return updated
}

// Later explicit fields are authoritative; omitted fields retain previously emitted evidence.
func mergeMetaOutputSnapshot(previous, latest []byte) []byte {
	if len(previous) == 0 {
		return bytes.Clone(latest)
	}
	var before, after map[string]json.RawMessage
	if json.Unmarshal(previous, &before) != nil || json.Unmarshal(latest, &after) != nil || after == nil {
		return bytes.Clone(latest)
	}
	changed := false
	for key, value := range before {
		if _, exists := after[key]; !exists {
			after[key] = value
			changed = true
		}
	}
	if !changed {
		return bytes.Clone(latest)
	}
	merged, err := json.Marshal(after)
	if err != nil {
		return bytes.Clone(latest)
	}
	return merged
}

func joinMetaOutputItems(items [][]byte) []byte {
	return append(append([]byte{'['}, bytes.Join(items, []byte{','})...), ']')
}

func (s *MetaResponsesOutput) patchTerminal(event []byte) []byte {
	if len(s.items) == 0 || !gjson.GetBytes(event, "response").IsObject() {
		return event
	}
	terminal := gjson.GetBytes(event, "response.output").Array()
	used := make([]bool, len(terminal))
	ordered := append([]*metaObservedOutputItem(nil), s.items...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].indexed != ordered[j].indexed {
			return ordered[i].indexed
		}
		return ordered[i].indexed && ordered[i].index < ordered[j].index
	})
	output := make([][]byte, 0, len(ordered)+len(terminal))
	for _, item := range ordered {
		snapshot := item.snapshot()
		id := gjson.GetBytes(snapshot, "id").String()
		matched := -1
		if id != "" {
			for i, candidate := range terminal {
				if !used[i] && candidate.Get("id").String() == id {
					matched = i
					break
				}
			}
		}
		if matched < 0 && item.indexed && item.index < int64(len(terminal)) {
			i := int(item.index)
			candidate := terminal[i]
			candidateID := candidate.Get("id").String()
			if !used[i] && candidate.IsObject() && (id == "" || candidateID == "" || id == candidateID) && candidate.Get("type").String() == gjson.GetBytes(snapshot, "type").String() {
				matched = i
			}
		}
		if matched >= 0 {
			snapshot = mergeMetaOutputSnapshot(snapshot, []byte(terminal[matched].Raw))
			used[matched] = true
		}
		output = append(output, snapshot)
	}
	for i, item := range terminal {
		if !used[i] {
			output = append(output, []byte(item.Raw))
		}
	}
	updated, err := sjson.SetRawBytes(event, "response.output", joinMetaOutputItems(output))
	if err != nil {
		return event
	}
	return updated
}

// nativeOutputTransformer adapts the fork's observed-output accumulator to runner events.
type nativeOutputTransformer struct{ MetaResponsesOutput }

func (s *nativeOutputTransformer) Reset() { s.MetaResponsesOutput = MetaResponsesOutput{} }
func (s *nativeOutputTransformer) Transform(event []byte) ([][]byte, error) {
	return [][]byte{s.MetaResponsesOutput.Transform(event)}, nil
}
func (s *nativeOutputTransformer) Finish() ([][]byte, error) { return nil, nil }
func (s *nativeOutputTransformer) TransformCompleted(event []byte) ([]byte, error) {
	return s.MetaResponsesOutput.Transform(event), nil
}
