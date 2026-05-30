package proxy

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestHasWebSearchTool(t *testing.T) {
	// Exactly one web_search tool -> true
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4",
		Messages: []ClaudeMessage{{Role: "user", Content: "test"}},
		Tools:    []ClaudeTool{{Name: "web_search"}},
	}
	if !hasWebSearchTool(req) {
		t.Fatal("expected hasWebSearchTool=true for a single web_search tool")
	}

	// Multiple tools -> false
	reqMulti := &ClaudeRequest{
		Model:    "claude-sonnet-4",
		Messages: []ClaudeMessage{{Role: "user", Content: "test"}},
		Tools:    []ClaudeTool{{Name: "web_search"}, {Name: "other_tool"}},
	}
	if hasWebSearchTool(reqMulti) {
		t.Fatal("expected hasWebSearchTool=false when more than one tool present")
	}

	// No tools -> false
	if hasWebSearchTool(&ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: "x"}}}) {
		t.Fatal("expected hasWebSearchTool=false with no tools")
	}

	// Single non-web_search tool -> false
	if hasWebSearchTool(&ClaudeRequest{Tools: []ClaudeTool{{Name: "calculator"}}}) {
		t.Fatal("expected hasWebSearchTool=false for non web_search tool")
	}
}

func TestExtractWebSearchQueryWithPrefix(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{{
			Role: "user",
			Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "Perform a web search for the query: rust latest version 2026"},
			},
		}},
	}
	q, ok := extractWebSearchQuery(req)
	if !ok || q != "rust latest version 2026" {
		t.Fatalf("expected prefix stripped, got %q ok=%v", q, ok)
	}
}

func TestExtractWebSearchQueryPlainString(t *testing.T) {
	req := &ClaudeRequest{
		Messages: []ClaudeMessage{{Role: "user", Content: "What is the weather today?"}},
	}
	q, ok := extractWebSearchQuery(req)
	if !ok || q != "What is the weather today?" {
		t.Fatalf("expected plain text, got %q ok=%v", q, ok)
	}
}

func TestExtractWebSearchQueryEmpty(t *testing.T) {
	// Empty content array -> not ok
	if _, ok := extractWebSearchQuery(&ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{}}}}); ok {
		t.Fatal("expected ok=false for empty content array")
	}
	// No messages -> not ok
	if _, ok := extractWebSearchQuery(&ClaudeRequest{}); ok {
		t.Fatal("expected ok=false for no messages")
	}
	// Prefix only (empty query after strip) -> not ok
	req := &ClaudeRequest{Messages: []ClaudeMessage{{Role: "user", Content: webSearchQueryPrefix}}}
	if _, ok := extractWebSearchQuery(req); ok {
		t.Fatal("expected ok=false when query is empty after stripping prefix")
	}
}

func TestNewMCPRequestIDFormat(t *testing.T) {
	id := newMCPRequestID()
	if !strings.HasPrefix(id, "web_search_tooluse_") {
		t.Fatalf("missing prefix: %q", id)
	}
	suffix := strings.TrimPrefix(id, "web_search_tooluse_")
	parts := strings.Split(suffix, "_")
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d: %v", len(parts), parts)
	}
	if len(parts[0]) != 22 {
		t.Fatalf("expected 22-char random, got %d", len(parts[0]))
	}
	if _, err := strconv.ParseInt(parts[1], 10, 64); err != nil {
		t.Fatalf("expected millis timestamp, got %q", parts[1])
	}
	if len(parts[2]) != 8 {
		t.Fatalf("expected 8-char random, got %d", len(parts[2]))
	}
}

func TestNewServerToolUseIDFormat(t *testing.T) {
	id := newServerToolUseID()
	if !strings.HasPrefix(id, "srvtoolu_") {
		t.Fatalf("missing srvtoolu_ prefix: %q", id)
	}
	if hex := strings.TrimPrefix(id, "srvtoolu_"); len(hex) != 32 {
		t.Fatalf("expected 32 hex chars, got %d", len(hex))
	}
}

func TestBuildMCPRequestBody(t *testing.T) {
	body, err := buildMCPRequestBody("req-1", "test query")
	if err != nil {
		t.Fatal(err)
	}
	var parsed mcpRequest
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.JSONRPC != "2.0" || parsed.Method != "tools/call" {
		t.Fatalf("unexpected jsonrpc/method: %+v", parsed)
	}
	if parsed.Params.Name != "web_search" || parsed.Params.Arguments.Query != "test query" {
		t.Fatalf("unexpected params: %+v", parsed.Params)
	}
}

func TestParseMCPSearchResults(t *testing.T) {
	body := `{"id":"x","jsonrpc":"2.0","result":{"isError":false,"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Test\",\"url\":\"https://example.com\",\"snippet\":\"Test snippet\"}],\"totalResults\":1}"}]}}`
	results, err := parseMCPSearchResults([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if results == nil || len(results.Results) != 1 {
		t.Fatalf("expected 1 result, got %+v", results)
	}
	if results.Results[0].Title != "Test" || results.Results[0].URL != "https://example.com" {
		t.Fatalf("unexpected result: %+v", results.Results[0])
	}
}

func TestParseMCPSearchResultsError(t *testing.T) {
	body := `{"id":"x","jsonrpc":"2.0","error":{"code":-32000,"message":"boom"}}`
	if _, err := parseMCPSearchResults([]byte(body)); err == nil {
		t.Fatal("expected error from MCP error response")
	}
}

func TestGenerateWebSearchSummary(t *testing.T) {
	snippet := "This is a test snippet"
	results := &webSearchResults{
		Results: []webSearchResult{{
			Title:   "Test Result",
			URL:     "https://example.com",
			Snippet: &snippet,
		}},
	}
	summary := generateWebSearchSummary("test", results)
	for _, want := range []string{"Test Result", "https://example.com", "This is a test snippet"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q: %s", want, summary)
		}
	}

	// No results -> "No results found."
	empty := generateWebSearchSummary("test", nil)
	if !strings.Contains(empty, "No results found.") {
		t.Fatalf("expected no-results message, got: %s", empty)
	}
}

func TestRuneTruncateMultibyte(t *testing.T) {
	// 5 Chinese characters; truncating to 3 must not split a rune.
	got := runeTruncate("你好世界吗", 3)
	if got != "你好世..." {
		t.Fatalf("expected rune-safe truncation, got %q", got)
	}
	if got := runeTruncate("abc", 10); got != "abc" {
		t.Fatalf("expected no truncation, got %q", got)
	}
}

func TestChunkByRunesMultibyte(t *testing.T) {
	// Emoji + CJK chunked by 2 runes; every chunk must remain valid UTF-8.
	chunks := chunkByRunes("🚀火箭发射", 2)
	rejoined := strings.Join(chunks, "")
	if rejoined != "🚀火箭发射" {
		t.Fatalf("rejoined mismatch: %q", rejoined)
	}
	for _, c := range chunks {
		if len([]rune(c)) > 2 {
			t.Fatalf("chunk exceeds 2 runes: %q", c)
		}
	}
}

func TestBuildWebSearchEventsSequence(t *testing.T) {
	snippet := "snippet text"
	results := &webSearchResults{
		Results: []webSearchResult{{Title: "T1", URL: "https://a.com", Snippet: &snippet}},
	}
	events := buildWebSearchEvents("claude-sonnet-4", "golang", "srvtoolu_abc", results, 42)

	if len(events) < 11 {
		t.Fatalf("expected at least 11 events, got %d", len(events))
	}

	// First event message_start, last message_stop.
	if events[0].Event != "message_start" {
		t.Fatalf("first event = %q", events[0].Event)
	}
	if events[len(events)-1].Event != "message_stop" {
		t.Fatalf("last event = %q", events[len(events)-1].Event)
	}

	// Collect the content_block_start blocks in order and verify types/indices.
	type startBlock struct {
		index int
		ctype string
	}
	var starts []startBlock
	var messageDelta map[string]interface{}
	for _, e := range events {
		switch e.Event {
		case "content_block_start":
			idx, _ := e.Data["index"].(int)
			cb, _ := e.Data["content_block"].(map[string]interface{})
			ct, _ := cb["type"].(string)
			starts = append(starts, startBlock{idx, ct})
		case "message_delta":
			messageDelta = e.Data
		}
	}

	want := []startBlock{
		{0, "text"},
		{1, "server_tool_use"},
		{2, "web_search_tool_result"},
		{3, "text"},
	}
	if len(starts) != len(want) {
		t.Fatalf("expected %d content blocks, got %d (%+v)", len(want), len(starts), starts)
	}
	for i, w := range want {
		if starts[i] != w {
			t.Fatalf("block %d = %+v, want %+v", i, starts[i], w)
		}
	}

	// server_tool_use must carry input in one shot at content_block_start.
	for _, e := range events {
		if e.Event != "content_block_start" {
			continue
		}
		cb, _ := e.Data["content_block"].(map[string]interface{})
		if ct, _ := cb["type"].(string); ct == "server_tool_use" {
			input, ok := cb["input"].(map[string]interface{})
			if !ok || input["query"] != "golang" {
				t.Fatalf("server_tool_use input not sent in one shot: %+v", cb)
			}
			if cb["id"] != "srvtoolu_abc" || cb["name"] != "web_search" {
				t.Fatalf("server_tool_use id/name mismatch: %+v", cb)
			}
		}
	}

	// message_delta must report web_search_requests and omit stop_sequence.
	if messageDelta == nil {
		t.Fatal("no message_delta event")
	}
	delta, _ := messageDelta["delta"].(map[string]interface{})
	if _, present := delta["stop_sequence"]; present {
		t.Fatal("message_delta.delta should not contain stop_sequence")
	}
	usage, _ := messageDelta["usage"].(map[string]interface{})
	stu, _ := usage["server_tool_use"].(map[string]interface{})
	if stu["web_search_requests"] != 1 {
		t.Fatalf("expected web_search_requests=1, got %+v", usage)
	}
}

func TestMCPURLForEndpoint(t *testing.T) {
	got := mcpURLForEndpoint("https://q.us-east-1.amazonaws.com/generateAssistantResponse")
	if got != "https://q.us-east-1.amazonaws.com/mcp" {
		t.Fatalf("unexpected mcp url: %q", got)
	}
}
