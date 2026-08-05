package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chaugan/tunnelhw/internal/relay"
	"github.com/chaugan/tunnelhw/internal/relayapi"
)

// newTestServer serves the MCP handler over a broker with no agents, with a
// stub verify: "rw-token" is full access, "ro-token" is read-only.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	b := relayapi.NewBroker(relay.NewHub(nil))
	verify := func(token string) ([]string, bool, bool) {
		switch token {
		case "rw-token":
			return nil, false, true
		case "ro-token":
			return nil, true, true
		default:
			return nil, false, false
		}
	}
	ts := httptest.NewServer(Handler(b, verify))
	t.Cleanup(ts.Close)
	return ts
}

// bearerRT injects a bearer token into every request.
type bearerRT struct{ token string }

func (rt bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+rt.token)
	return http.DefaultTransport.RoundTrip(r)
}

// dial connects a real SDK client through the handler.
func dial(t *testing.T, url, token string) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	tr := &sdk.StreamableClientTransport{
		Endpoint:             url,
		HTTPClient:           &http.Client{Transport: bearerRT{token: token}},
		DisableStandaloneSSE: true,
	}
	cs, err := client.Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestHandlerRejectsBadAuth(t *testing.T) {
	ts := newTestServer(t)
	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong scheme", "Basic abc"},
		{"unknown token", "Bearer nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			var body map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode 401 body: %v", err)
			}
			if body["error"] == "" {
				t.Fatal("401 body has no error field")
			}
		})
	}
}

func TestListToolsAndListDevicesEmpty(t *testing.T) {
	ts := newTestServer(t)
	cs := dial(t, ts.URL, "rw-token")

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		"list_devices": false, "open_device": false, "read": false, "write": false,
		"set_params": false, "drain": false, "close_session": false,
	}
	for _, tool := range tools.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		want[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has no description", tool.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q missing", name)
		}
	}

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "list_devices"})
	if err != nil {
		t.Fatalf("CallTool list_devices: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_devices errored: %+v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Devices []any `json:"devices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content %s: %v", raw, err)
	}
	if out.Devices == nil {
		t.Fatalf("devices is null, want empty list; got %s", raw)
	}
	if len(out.Devices) != 0 {
		t.Fatalf("devices = %s, want empty", raw)
	}
}

func TestReadOnlyTokenForbidsMutations(t *testing.T) {
	ts := newTestServer(t)
	cs := dial(t, ts.URL, "ro-token")

	mutating := []struct {
		tool string
		args map[string]any
	}{
		{"open_device", map[string]any{"device_id": "amber-falcon"}},
		{"write", map[string]any{"session_id": "s-1", "data": "x"}},
		{"set_params", map[string]any{"session_id": "s-1"}},
		{"drain", map[string]any{"session_id": "s-1"}},
		{"close_session", map[string]any{"session_id": "s-1"}},
	}
	for _, tc := range mutating {
		t.Run(tc.tool, func(t *testing.T) {
			res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool %s: %v", tc.tool, err)
			}
			if !res.IsError {
				t.Fatalf("%s under read-only token did not error", tc.tool)
			}
			text := contentText(res)
			if !strings.Contains(text, "read-only") {
				t.Fatalf("%s error %q does not mention read-only", tc.tool, text)
			}
		})
	}
}

func TestUnknownSessionIsToolError(t *testing.T) {
	ts := newTestServer(t)
	cs := dial(t, ts.URL, "rw-token")

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "read",
		Arguments: map[string]any{"session_id": "no-such", "timeout_ms": 1},
	})
	if err != nil {
		t.Fatalf("CallTool read: %v", err)
	}
	if !res.IsError {
		t.Fatal("read on unknown session did not error")
	}
	if text := contentText(res); !strings.Contains(text, "unknown session") {
		t.Fatalf("error %q does not mention unknown session", text)
	}
}

func contentText(res *sdk.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
