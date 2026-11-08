package muse

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestToolSchemaIntegerTypesSourceFields(t *testing.T) {
	// Paths follow the input schemas and Deserialize types in codex-rs core and ext.
	tests := []struct {
		name   string
		fields []string
	}{
		{"test_sync_tool", []string{"barrier.properties.participants", "barrier.properties.timeout_ms"}},
		{"create_goal", []string{"token_budget"}},
		{"get_channels", []string{"limit"}},
		{"list_threads", []string{"limit", "max_chars_per_post"}},
		{"search_posts", []string{"limit", "max_chars_per_post"}},
		{"read_thread", []string{"limit", "max_chars_per_post"}},
		{"read_post", []string{"offset_chars", "limit_chars"}},
		{"memories__list", []string{"max_results"}},
		{"memories__read", []string{"line_offset", "max_lines"}},
		{"memories__search", []string{"context_lines", "max_results"}},
		{"history__list_windows", []string{"limit"}},
		{"history__list_items", []string{"limit", "max_chars_per_item"}},
		{"history__read_item", []string{"offset_chars", "limit_chars"}},
		{"history__search_contents", []string{"limit"}},
		{"notes__list_files_by_prefix", []string{"max_results"}},
		{"notes__read_file", []string{"start_line", "stop_line", "start_line.anyOf.0", "stop_line.anyOf.0"}},
		{"notes__search_contents", []string{"max_matches_per_file", "max_files"}},
		{"image_gen__imagegen", []string{"num_last_images_to_include"}},
		{"web__run", []string{
			"search_query.items.properties.recency", "image_query.items.properties.recency",
			"open.items.properties.lineno", "click.items.properties.id",
			"screenshot.items.properties.pageno", "weather.items.properties.duration",
			"sports.items.properties.num_games",
		}},
		{"collaboration__wait_agent", []string{"timeout_ms"}},
		{"multi_agent_v1__wait_agent", []string{"timeout_ms"}},
		{"functions__create_goal", []string{"token_budget"}},
		{"collab__read_post", []string{"offset_chars", "limit_chars"}},
		{"collaboration__get_channels", []string{"limit"}},
		{"collaboration__list_threads", []string{"limit", "max_chars_per_post"}},
		{"collaboration__search_posts", []string{"limit", "max_chars_per_post"}},
		{"collaboration__read_thread", []string{"limit", "max_chars_per_post"}},
		{"collaboration__read_post", []string{"offset_chars", "limit_chars"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, typeJSON := range []string{`"number"`, `["number","integer","null"]`, `"integer"`, `"string"`} {
				input := []byte(fmt.Sprintf(`{"tools":[{"type":"function","name":%q,"parameters":{"type":"object","properties":{"unrelated":{"type":"number","default":1.5}}},"output_schema":{"properties":{"wall_time_seconds":{"type":"number"}}}}],"input":[{"type":"function_call","arguments":"{\"limit\":1.5}"}]}`, tt.name))
				for _, field := range tt.fields {
					input = setIntegerTestRaw(t, input, "tools.0.parameters.properties."+field, `{"type":`+typeJSON+`,"description":"Keep metadata","minimum":0,"default":1}`)
				}
				for _, ua := range []string{"codex-tui/0.154.0", "curl/8.7.1", ""} {
					var headers http.Header
					if ua != "" {
						headers = http.Header{"user-agent": []string{ua}}
					}
					want := bytes.Clone(input)
					if ua == "codex-tui/0.154.0" {
						wantType := typeJSON
						if typeJSON == `"number"` {
							wantType = `"integer"`
						} else if typeJSON == `["number","integer","null"]` {
							wantType = `["integer","null"]`
						}
						for _, field := range tt.fields {
							want = setIntegerTestRaw(t, want, "tools.0.parameters.properties."+field+".type", wantType)
						}
					}
					out := normalizeCodexToolIntegerTypes(input, headers)
					if !bytes.Equal(out, want) {
						t.Fatalf("UA %q type %s: got %s, want %s", ua, typeJSON, out, want)
					}
					if again := normalizeCodexToolIntegerTypes(out, headers); !bytes.Equal(again, out) {
						t.Fatal("normalization is not idempotent")
					}
				}
			}
		})
	}
}

func TestToolSchemaIntegerTypesNamespaceFormats(t *testing.T) {
	schema := `{"type":"object","properties":{"line_offset":{"type":"number"},"max_lines":{"type":["number","null"]},"max_tokens":{"type":"number"}}}`
	tests := []struct {
		name string
		body string
		path string
	}{
		{"responses", `{"tools":[{"type":"function","name":"memories__read","parameters":%s}]}`, "tools.0.parameters"},
		{"chat", `{"tools":[{"type":"function","function":{"name":"memories__read","parameters":%s}}]}`, "tools.0.function.parameters"},
		{"claude", `{"tools":[{"name":"memories__read","input_schema":%s}]}`, "tools.0.input_schema"},
		{"gemini", `{"tools":[{"function_declarations":[{"name":"memories__read","parameters":%s}]}]}`, "tools.0.function_declarations.0.parameters"},
		{"gemini JSON schema", `{"tools":[{"functionDeclarations":[{"name":"memories__read","parametersJsonSchema":%s}]}]}`, "tools.0.functionDeclarations.0.parametersJsonSchema"},
		{"namespace", `{"tools":[{"type":"namespace","name":"memories","tools":[{"type":"function","name":"read","parameters":%s}]}]}`, "tools.0.tools.0.parameters"},
		{"additional namespace", `{"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"memories","tools":[{"type":"function","name":"read","parameters":%s}]}]}]}`, "input.0.tools.0.tools.0.parameters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := []byte(fmt.Sprintf(tt.body, schema))
			want := setIntegerTestRaw(t, input, tt.path+".properties.line_offset.type", `"integer"`)
			want = setIntegerTestRaw(t, want, tt.path+".properties.max_lines.type", `["integer","null"]`)
			out := normalizeCodexToolIntegerTypes(input, http.Header{"User-Agent": []string{"Codex/1.0"}})
			if !bytes.Equal(out, want) {
				t.Fatalf("got %s, want %s", out, want)
			}
		})
	}
}

func TestToolSchemaIntegerTypesPreservesUnprovenFields(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
	}{
		{"unknown_tool", ""}, {"mcp__server__read_post", ""},
		{"read", ""}, {"run", ""}, {"imagegen", ""},
		{"read_post", "user_tools"}, {"wait_agent", "mcp__server"},
		{"read", "skills"}, {"read", "user_tools"},
		{"read_post", "multi_agent_v1"}, {"read_post", "arbitrary_collaboration"},
	}
	for _, tt := range tests {
		t.Run(tt.namespace+"/"+tt.name, func(t *testing.T) {
			tool := fmt.Sprintf(`{"type":"function","name":%q,"parameters":{"properties":{"limit":{"type":"number"},"offset_chars":{"type":"number"},"line_offset":{"type":"number"},"timeout_ms":{"type":"number"}}}}`, tt.name)
			if tt.namespace != "" {
				tool = fmt.Sprintf(`{"type":"namespace","name":%q,"tools":[%s]}`, tt.namespace, tool)
			}
			input := []byte(`{"tools":[` + tool + `]}`)
			out := normalizeCodexToolIntegerTypes(input, http.Header{"User-Agent": []string{"codex"}})
			if !bytes.Equal(out, input) {
				t.Fatalf("unknown tool changed: %s", out)
			}
		})
	}
	input := []byte(`{"tools":[{"name":"test_sync_tool","parameters":{"properties":{"barrier.participants":{"type":"number"},"other":{"properties":{"participants":{"type":"number"}}},"barrier":{"properties":{"participants":{"type":"number"},"ratio":{"type":"number"}}}}}}]}`)
	out := normalizeCodexToolIntegerTypes(input, http.Header{"User-Agent": []string{"codex"}})
	want := setIntegerTestRaw(t, input, "tools.0.parameters.properties.barrier.properties.participants.type", `"integer"`)
	if !bytes.Equal(out, want) || !gjson.ValidBytes(out) {
		t.Fatalf("nested path changed unrelated fields: %s", out)
	}
}

func TestToolSchemaIntegerTypesHistoryNotesSchemas(t *testing.T) {
	// Parameter schemas copied from codex-rs/ext/history-notes/src/tools.rs:143-218.
	fixture := []byte(toolSchemaHistoryNotesFixture)
	if !gjson.ValidBytes(fixture) {
		t.Fatal("invalid source schema fixture")
	}
	for _, namespace := range gjson.GetBytes(fixture, "tools").Array() {
		tool := namespace.Get("tools.0")
		name := namespace.Get("name").String() + "__" + tool.Get("name").String()
		t.Run(name, func(t *testing.T) {
			formats := []struct {
				name string
				body string
			}{
				{"namespace", `{"tools":[` + namespace.Raw + `]}`},
				{"flat", fmt.Sprintf(`{"tools":[{"name":%q,"parameters":%s}]}`, name, tool.Get("parameters").Raw)},
				{"additional", `{"input":[{"type":"additional_tools","tools":[` + namespace.Raw + `]}]}`},
			}
			for _, format := range formats {
				t.Run(format.name, func(t *testing.T) {
					// Simulate integer declarations degraded to number, retaining the real schema shape.
					input := []byte(strings.ReplaceAll(format.body, `"type": "integer"`, `"type": "number"`))
					for _, ua := range []string{"codex", "curl", ""} {
						want := input
						if ua == "codex" {
							want = []byte(format.body)
						}
						out := normalizeCodexToolIntegerTypes(input, http.Header{"User-Agent": []string{ua}})
						if !bytes.Equal(out, want) {
							t.Fatalf("UA %q: got %s, want %s", ua, out, want)
						}
						if again := normalizeCodexToolIntegerTypes(out, http.Header{"User-Agent": []string{ua}}); !bytes.Equal(again, out) {
							t.Fatal("source schema is not stable")
						}
					}
				})
			}
			for _, unknownName := range []string{tool.Get("name").String(), "mcp__server__" + name, "user__" + name} {
				input := []byte(fmt.Sprintf(`{"tools":[{"name":%q,"parameters":%s}]}`, unknownName, strings.ReplaceAll(tool.Get("parameters").Raw, `"type": "integer"`, `"type": "number"`)))
				if out := normalizeCodexToolIntegerTypes(input, http.Header{"User-Agent": []string{"codex"}}); !bytes.Equal(out, input) {
					t.Fatalf("unknown tool %q changed: %s", unknownName, out)
				}
			}
		})
	}
}

func TestToolSchemaIntegerTypesNotesExplicitUnionPaths(t *testing.T) {
	input := []byte(`{"tools":[{"name":"notes__read_file","parameters":{"type":"object","properties":{"start_line":{"anyOf":[{"type":"number"},{"type":"null"}],"default":-3},"stop_line":{"anyOf":[{"type":"number"},{"type":"number"}],"default":-1},"ratio":{"anyOf":[{"type":"number"},{"type":"null"}]},"other":{"properties":{"start_line":{"anyOf":[{"type":"number"}]}}}},"required":["path"]}}],"input":[{"type":"function_call","arguments":"{\"start_line\":-3,\"stop_line\":-1}"}]}`)
	want := setIntegerTestRaw(t, input, "tools.0.parameters.properties.start_line.anyOf.0.type", `"integer"`)
	want = setIntegerTestRaw(t, want, "tools.0.parameters.properties.stop_line.anyOf.0.type", `"integer"`)
	out := normalizeCodexToolIntegerTypes(input, http.Header{"User-Agent": []string{"codex"}})
	if !bytes.Equal(out, want) {
		t.Fatalf("explicit union paths changed unrelated branches or signed values: %s", out)
	}
}

func setIntegerTestRaw(t *testing.T, body []byte, path, value string) []byte {
	t.Helper()
	out, errSet := sjson.SetRawBytes(body, path, []byte(value))
	if errSet != nil {
		t.Fatal(errSet)
	}
	return out
}

// Source fixture ported from fork internal/client/codex/tool-schema/testdata/history_notes_tools.json.
const toolSchemaHistoryNotesFixture = "{\n    \"tools\": [\n        {\n            \"type\": \"namespace\",\n            \"name\": \"history\",\n            \"tools\": [\n                {\n                    \"type\": \"function\",\n                    \"name\": \"list_windows\",\n                    \"parameters\": {\n                        \"type\": \"object\",\n                        \"properties\": {\n                            \"limit\": {\n                                \"type\": \"integer\",\n                                \"minimum\": 1,\n                                \"description\": \"Maximum number of windows to return.\"\n                            },\n                            \"agent_name\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Agent whose history to inspect. Omit to use the current agent; otherwise pass an absolute agent name or a name relative to the current agent.\"\n                            },\n                            \"recent_first\": {\n                                \"type\": \"boolean\",\n                                \"description\": \"Whether to return the most recently created windows first.\"\n                            }\n                        }\n                    }\n                }\n            ]\n        },\n        {\n            \"type\": \"namespace\",\n            \"name\": \"history\",\n            \"tools\": [\n                {\n                    \"type\": \"function\",\n                    \"name\": \"list_items\",\n                    \"parameters\": {\n                        \"type\": \"object\",\n                        \"properties\": {\n                            \"limit\": {\n                                \"type\": \"integer\",\n                                \"minimum\": 1,\n                                \"description\": \"Maximum number of items to return.\"\n                            },\n                            \"recent_first\": {\n                                \"type\": \"boolean\",\n                                \"description\": \"Whether to return the most recently created items first.\"\n                            },\n                            \"tool_namespace\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Callable namespace to include. When set, non-tool messages are excluded.\"\n                            },\n                            \"role\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\",\n                                        \"enum\": [\n                                            \"user\",\n                                            \"assistant\",\n                                            \"tool\",\n                                            \"system\",\n                                            \"developer\"\n                                        ]\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Message role to include. Null or omission includes all roles.\"\n                            },\n                            \"agent_name\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Agent whose history to inspect. Omit to use the current agent; otherwise pass an absolute agent name or a name relative to the current agent.\"\n                            },\n                            \"tool_name\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Callable tool name to include. When set, non-tool messages are excluded.\"\n                            },\n                            \"window_id\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Full window ID. Null or omission includes all windows.\"\n                            },\n                            \"max_chars_per_item\": {\n                                \"type\": \"integer\",\n                                \"minimum\": 1,\n                                \"description\": \"Maximum characters returned in each item's truncated_content.\"\n                            }\n                        }\n                    }\n                }\n            ]\n        },\n        {\n            \"type\": \"namespace\",\n            \"name\": \"history\",\n            \"tools\": [\n                {\n                    \"type\": \"function\",\n                    \"name\": \"read_item\",\n                    \"parameters\": {\n                        \"type\": \"object\",\n                        \"properties\": {\n                            \"agent_name\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Agent whose history to inspect. Omit to use the current agent; otherwise pass an absolute agent name or a name relative to the current agent.\"\n                            },\n                            \"item_id\": {\n                                \"type\": \"string\",\n                                \"description\": \"The short item ID is the suffix shown in the target item's trailing `[id: ...]` marker, printed after that item's content.\"\n                            },\n                            \"offset_chars\": {\n                                \"type\": \"integer\",\n                                \"minimum\": 0,\n                                \"description\": \"Zero-based character offset at which reading starts.\"\n                            },\n                            \"limit_chars\": {\n                                \"type\": \"integer\",\n                                \"minimum\": 1,\n                                \"description\": \"Maximum number of characters to return.\"\n                            },\n                            \"window_id\": {\n                                \"type\": \"string\",\n                                \"description\": \"Full window ID containing the item.\"\n                            }\n                        },\n                        \"required\": [\n                            \"item_id\",\n                            \"window_id\"\n                        ]\n                    }\n                }\n            ]\n        },\n        {\n            \"type\": \"namespace\",\n            \"name\": \"history\",\n            \"tools\": [\n                {\n                    \"type\": \"function\",\n                    \"name\": \"search_contents\",\n                    \"parameters\": {\n                        \"type\": \"object\",\n                        \"properties\": {\n                            \"limit\": {\n                                \"type\": \"integer\",\n                                \"minimum\": 1,\n                                \"description\": \"Maximum number of matching items to return.\"\n                            },\n                            \"query\": {\n                                \"type\": \"string\",\n                                \"encrypted\": true,\n                                \"description\": \"Case-sensitive literal substring to find in item content.\"\n                            },\n                            \"recent_first\": {\n                                \"type\": \"boolean\",\n                                \"description\": \"Whether to return the most recently created matches first.\"\n                            },\n                            \"tool_namespace\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Callable namespace to include. When set, non-tool messages are excluded.\"\n                            },\n                            \"role\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\",\n                                        \"enum\": [\n                                            \"user\",\n                                            \"assistant\",\n                                            \"tool\",\n                                            \"system\",\n                                            \"developer\"\n                                        ]\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Message role to include. Null or omission includes all roles.\"\n                            },\n                            \"agent_name\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Agent whose history to inspect. Omit to use the current agent; otherwise pass an absolute agent name or a name relative to the current agent.\"\n                            },\n                            \"tool_name\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Callable tool name to include. When set, non-tool messages are excluded.\"\n                            },\n                            \"window_id\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Full window ID. Null or omission includes all windows.\"\n                            }\n                        },\n                        \"required\": [\n                            \"query\"\n                        ]\n                    }\n                }\n            ]\n        },\n        {\n            \"type\": \"namespace\",\n            \"name\": \"notes\",\n            \"tools\": [\n                {\n                    \"type\": \"function\",\n                    \"name\": \"list_files_by_prefix\",\n                    \"parameters\": {\n                        \"type\": \"object\",\n                        \"properties\": {\n                            \"prefix\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Note path prefix to list.\"\n                            },\n                            \"max_results\": {\n                                \"type\": \"integer\",\n                                \"minimum\": 1,\n                                \"description\": \"Maximum number of files to return.\"\n                            },\n                            \"file_order_by\": {\n                                \"type\": \"string\",\n                                \"enum\": [\n                                    \"name\",\n                                    \"created_at\",\n                                    \"updated_at\"\n                                ],\n                                \"description\": \"Field used to order files.\"\n                            },\n                            \"file_order\": {\n                                \"type\": \"string\",\n                                \"enum\": [\n                                    \"ascending\",\n                                    \"descending\"\n                                ],\n                                \"description\": \"Direction used to order files.\"\n                            }\n                        }\n                    }\n                }\n            ]\n        },\n        {\n            \"type\": \"namespace\",\n            \"name\": \"notes\",\n            \"tools\": [\n                {\n                    \"type\": \"function\",\n                    \"name\": \"read_file\",\n                    \"parameters\": {\n                        \"type\": \"object\",\n                        \"properties\": {\n                            \"path\": {\n                                \"type\": \"string\",\n                                \"description\": \"Note file path to read.\"\n                            },\n                            \"start_line\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"integer\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"First line to return, inclusive and 1-based. Negative values count backward from the final line.\"\n                            },\n                            \"stop_line\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"integer\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Last line to return, inclusive and 1-based. Negative values count backward from the final line.\"\n                            }\n                        },\n                        \"required\": [\n                            \"path\"\n                        ]\n                    }\n                }\n            ]\n        },\n        {\n            \"type\": \"namespace\",\n            \"name\": \"notes\",\n            \"tools\": [\n                {\n                    \"type\": \"function\",\n                    \"name\": \"search_contents\",\n                    \"parameters\": {\n                        \"type\": \"object\",\n                        \"properties\": {\n                            \"max_matches_per_file\": {\n                                \"type\": \"integer\",\n                                \"minimum\": 1,\n                                \"description\": \"Maximum number of matching lines returned per file.\"\n                            },\n                            \"query\": {\n                                \"type\": \"string\",\n                                \"encrypted\": true,\n                                \"description\": \"Case-sensitive literal substring to find in note lines.\"\n                            },\n                            \"recent_file_first\": {\n                                \"type\": \"boolean\",\n                                \"description\": \"Whether to order matching files by creation time, newest first.\"\n                            },\n                            \"max_files\": {\n                                \"type\": \"integer\",\n                                \"minimum\": 1,\n                                \"description\": \"Maximum number of matching files returned.\"\n                            },\n                            \"path_prefix\": {\n                                \"anyOf\": [\n                                    {\n                                        \"type\": \"string\"\n                                    },\n                                    {\n                                        \"type\": \"null\"\n                                    }\n                                ],\n                                \"description\": \"Note path prefix to search.\"\n                            }\n                        },\n                        \"required\": [\n                            \"query\"\n                        ]\n                    }\n                }\n            ]\n        }\n    ]\n}\n"
