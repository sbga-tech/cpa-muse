package muse

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Ported from fork internal/util/responses_tools.go. Only the declaration
// collection used by the Meta apply_patch path is retained; client-executed
// tool-search discovery is not enabled by CollectResponsesToolDescriptors.
type applyPatchToolDescriptor struct {
	Name, LocalName, Namespace string
	Tool                       gjson.Result
	SourcePriority             int
	Direct                     bool
	Order                      int
}

func qualifyApplyPatchToolName(namespaceName, childName string) string {
	childName = strings.TrimSpace(childName)
	namespaceName = strings.TrimSpace(namespaceName)
	if childName == "" || namespaceName == "" || strings.HasPrefix(childName, "mcp__") {
		return childName
	}
	if childName == namespaceName || strings.HasPrefix(childName, namespaceName+"__") {
		return childName
	}
	if strings.HasSuffix(namespaceName, "__") {
		return namespaceName + childName
	}
	return namespaceName + "__" + childName
}

func collectApplyPatchToolDescriptors(root gjson.Result) []applyPatchToolDescriptor {
	var descriptors []applyPatchToolDescriptor
	nameOf := func(tool gjson.Result) string {
		if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
			return name
		}
		return strings.TrimSpace(tool.Get("function.name").String())
	}
	appendDescriptor := func(tool gjson.Result, namespace string, priority int, direct bool) {
		name := nameOf(tool)
		if name == "" {
			return
		}
		descriptors = append(descriptors, applyPatchToolDescriptor{
			Name: qualifyApplyPatchToolName(namespace, name), LocalName: name,
			Namespace: namespace, Tool: tool, SourcePriority: priority,
			Direct: direct, Order: len(descriptors),
		})
	}
	appendTools := func(tools gjson.Result, priority int) {
		if !tools.IsArray() {
			return
		}
		for _, tool := range tools.Array() {
			switch strings.TrimSpace(tool.Get("type").String()) {
			case "", "function", "custom":
				appendDescriptor(tool, "", priority, true)
			case "namespace":
				namespace := strings.TrimSpace(tool.Get("name").String())
				children := tool.Get("tools")
				if !children.IsArray() {
					children = tool.Get("children")
				}
				if !children.IsArray() {
					continue
				}
				for _, child := range children.Array() {
					switch strings.TrimSpace(child.Get("type").String()) {
					case "", "function", "custom":
						appendDescriptor(child, namespace, priority, false)
					}
				}
			}
		}
	}
	appendTools(root.Get("tools"), 0)
	if !root.Get("input").IsArray() {
		return descriptors
	}
	for _, item := range root.Get("input").Array() {
		if item.Get("type").String() == "additional_tools" {
			appendTools(item.Get("tools"), 1)
		}
	}
	return descriptors
}

func collectApplyPatchToolWinners(root gjson.Result) map[string]applyPatchToolDescriptor {
	winners := map[string]applyPatchToolDescriptor{}
	for _, descriptor := range collectApplyPatchToolDescriptors(root) {
		current, exists := winners[descriptor.Name]
		precedes := descriptor.SourcePriority < current.SourcePriority
		if descriptor.SourcePriority == current.SourcePriority {
			precedes = descriptor.Direct && !current.Direct || descriptor.Direct == current.Direct && descriptor.Order < current.Order
		}
		if !exists || precedes {
			winners[descriptor.Name] = descriptor
		}
	}
	return winners
}

// Ported from fork internal/translator/common/bytes.go.
func joinApplyPatchRawArray(items [][]byte) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	size := len(items) + 1
	for _, item := range items {
		size += len(item)
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for i, item := range items {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	return append(out, ']')
}
