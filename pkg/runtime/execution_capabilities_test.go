package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func nativeExecutionProfile() *ExecutionCapabilities {
	return &ExecutionCapabilities{
		Mode: "native", ToolExecution: "docker-agent", ToolApproval: "docker-agent-policy",
		BudgetSupported: true, CompactionSupported: true, MidTurnSteeringSupported: true, PromptHookContextSupported: true,
	}
}

func harnessExecutionProfile() *ExecutionCapabilities {
	return &ExecutionCapabilities{Mode: "harness", ToolExecution: "harness-reported", ToolApproval: "external"}
}

func TestExecutionCapabilitiesProfiles(t *testing.T) {
	t.Parallel()
	assert.Nil(t, executionCapabilities(nil))
	assert.Nil(t, (*LocalRuntime)(nil).AgentExecutionCapabilities("native"))
	assert.Nil(t, (&LocalRuntime{}).AgentExecutionCapabilities("native"))
	assert.Equal(t, nativeExecutionProfile(), executionCapabilities(agent.New("native", "")))
	assert.Equal(t, harnessExecutionProfile(), executionCapabilities(agent.New("harness", "", agent.WithHarness(&latest.HarnessConfig{Type: "codex"}))))
	profile := executionCapabilities(agent.New("native", ""))
	profile.ToolApproval = "changed"
	assert.Equal(t, nativeExecutionProfile(), executionCapabilities(agent.New("native", "")), "profiles must not share mutable values")
}

type capabilityProbeToolset struct {
	calls int
}

func (s *capabilityProbeToolset) Start(context.Context) error { s.calls++; return nil }
func (s *capabilityProbeToolset) Stop(context.Context) error  { return nil }
func (s *capabilityProbeToolset) Tools(context.Context) ([]tools.Tool, error) {
	s.calls++
	return nil, nil
}

func TestExecutionCapabilitiesInspectionIsLazy(t *testing.T) {
	t.Parallel()
	toolset := &capabilityProbeToolset{}
	native := agent.New("native", "", agent.WithModel(&mockProvider{id: "test/model"}), agent.WithToolSets(toolset))
	external := agent.New("external", "", agent.WithHarness(&latest.HarnessConfig{Type: "codex"}), agent.WithToolSets(toolset))
	factoryCalls := 0
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(native, external)),
		WithModelStore(mockModelStore{}), WithSessionCompaction(false),
		WithBudget(&latest.BudgetConfig{MaxTokens: 10}),
		WithHarnessFactory(func(*latest.HarnessConfig) (harness.Provider, error) {
			factoryCalls++
			return nil, nil
		}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	for _, tc := range []struct {
		name string
		want *ExecutionCapabilities
	}{
		{"native", nativeExecutionProfile()}, {"external", harnessExecutionProfile()}, {"missing", nil},
	} {
		assert.Equal(t, tc.want, rt.AgentExecutionCapabilities(tc.name))
		assert.Equal(t, tc.want, rt.AgentConfigInfo(t.Context(), tc.name).ExecutionCapabilities)
	}
	var events []Event
	rt.EmitAgentInfo(t.Context(), EventSinkFunc(func(event Event) { events = append(events, event) }))
	require.Len(t, events, 2)
	assert.Equal(t, nativeExecutionProfile(), events[0].(*AgentInfoEvent).ExecutionCapabilities)
	roster := events[1].(*TeamInfoEvent)
	require.Len(t, roster.AvailableAgents, 2)
	assert.Equal(t, nativeExecutionProfile(), roster.AvailableAgents[0].ExecutionCapabilities)
	assert.Equal(t, harnessExecutionProfile(), roster.AvailableAgents[1].ExecutionCapabilities)
	assert.Zero(t, toolset.calls)
	assert.Zero(t, factoryCalls)
	assert.False(t, rt.budgetStarted, "inspection must not initialize budget counters")

	require.NoError(t, rt.SetCurrentAgent(t.Context(), "external"))
	events = nil
	rt.EmitAgentInfo(t.Context(), EventSinkFunc(func(event Event) { events = append(events, event) }))
	assert.Equal(t, harnessExecutionProfile(), events[0].(*AgentInfoEvent).ExecutionCapabilities)
}

func TestExecutionCapabilitiesStreamRoster(t *testing.T) {
	t.Parallel()
	root := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/model", stream: newStreamBuilder().AddContent("reply").AddStopWithUsage(1, 1).Build()}))
	external := agent.New("external", "", agent.WithHarness(&latest.HarnessConfig{Type: "codex"}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, external)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	var roster *TeamInfoEvent
	for event := range rt.RunStream(t.Context(), session.New(session.WithUserMessage("hello"))) {
		if info, ok := event.(*TeamInfoEvent); ok {
			roster = info
		}
	}
	require.NotNil(t, roster)
	require.Len(t, roster.AvailableAgents, 2)
	assert.Equal(t, nativeExecutionProfile(), roster.AvailableAgents[0].ExecutionCapabilities)
	assert.Equal(t, harnessExecutionProfile(), roster.AvailableAgents[1].ExecutionCapabilities)
}

func TestExecutionCapabilitiesClientDecodesRoster(t *testing.T) {
	t.Parallel()
	roster := TeamInfo([]AgentDetails{
		{Name: "native", ExecutionCapabilities: nativeExecutionProfile()},
		{Name: "external", ExecutionCapabilities: harnessExecutionProfile()},
		{Name: "legacy"},
	}, "native")
	payload, err := json.Marshal(roster)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL)
	require.NoError(t, err)
	stream, err := client.RunAgent(t.Context(), "session", "team", nil, "")
	require.NoError(t, err)
	var decoded *TeamInfoEvent
	for event := range stream {
		if info, ok := event.(*TeamInfoEvent); ok {
			decoded = info
		}
	}
	require.NotNil(t, decoded)
	want := roster.(*TeamInfoEvent)
	assert.Equal(t, want.Type, decoded.Type)
	assert.Equal(t, want.CurrentAgent, decoded.CurrentAgent)
	assert.Equal(t, want.AvailableAgents, decoded.AvailableAgents)
	assert.True(t, want.Timestamp.Equal(decoded.Timestamp))
}

func TestExecutionCapabilitiesJSONCompatibility(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		profile *ExecutionCapabilities
	}{
		{"unknown", nil}, {"native", nativeExecutionProfile()}, {"harness", harnessExecutionProfile()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			original := AgentDetails{Name: "root", ExecutionCapabilities: tc.profile}
			data, err := json.Marshal(original)
			require.NoError(t, err)
			var decoded AgentDetails
			require.NoError(t, json.Unmarshal(data, &decoded))
			assert.Equal(t, original, decoded)
			if tc.profile == nil {
				assert.NotContains(t, string(data), "execution_capabilities")
			} else if tc.name == "harness" {
				assert.Contains(t, string(data), `"budget_supported":false`)
				assert.Contains(t, string(data), `"prompt_hook_context_supported":false`)
			}
		})
	}
	var legacy TeamInfoEvent
	require.NoError(t, json.Unmarshal([]byte(`{"type":"team_info","available_agents":[{"name":"root","model":"codex"}],"current_agent":"root"}`), &legacy))
	require.Len(t, legacy.AvailableAgents, 1)
	assert.Nil(t, legacy.AvailableAgents[0].ExecutionCapabilities)
	update := AgentInfo("root", "test/model", "", "")
	data, err := json.Marshal(update)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "execution_capabilities", "model-only updates must not guess capabilities")
}

type capabilityRemoteClient struct {
	stubRemoteClient

	events []Event
}

func (c *capabilityRemoteClient) RunAgent(context.Context, string, string, []api.Message, string) (<-chan Event, error) {
	out := make(chan Event, len(c.events))
	for _, event := range c.events {
		out <- event
	}
	close(out)
	return out, nil
}

func TestExecutionCapabilitiesRemoteDoesNotInferSupport(t *testing.T) {
	t.Parallel()
	for _, capabilities := range []*ExecutionCapabilities{nil, harnessExecutionProfile()} {
		t.Run(fmt.Sprintf("known=%t", capabilities != nil), func(t *testing.T) {
			t.Parallel()
			roster := TeamInfo([]AgentDetails{{Name: "root", ExecutionCapabilities: capabilities}}, "root")
			client := &capabilityRemoteClient{
				stubRemoteClient: stubRemoteClient{cfg: &latest.Config{Agents: latest.Agents{{Name: "root", Harness: &latest.HarnessConfig{Type: "codex"}}}}},
				events:           []Event{roster, StreamStopped("session", "root", "completed")},
			}
			rt, err := NewRemoteRuntime(client)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			rt.EmitAgentInfo(t.Context(), EventSinkFunc(func(event Event) {
				switch info := event.(type) {
				case *AgentInfoEvent:
					assert.Nil(t, info.ExecutionCapabilities)
				case *TeamInfoEvent:
					assert.Nil(t, info.AvailableAgents[0].ExecutionCapabilities)
				}
			}))
			var forwarded *TeamInfoEvent
			for event := range rt.RunStream(t.Context(), session.New(session.WithID("session"))) {
				if info, ok := event.(*TeamInfoEvent); ok {
					forwarded = info
				}
			}
			require.NotNil(t, forwarded)
			assert.Same(t, roster, forwarded)
			assert.Equal(t, capabilities, forwarded.AvailableAgents[0].ExecutionCapabilities)
		})
	}
}
