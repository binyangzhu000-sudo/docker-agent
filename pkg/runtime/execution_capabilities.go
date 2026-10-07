package runtime

import "github.com/docker/docker-agent/pkg/agent"

const (
	ExecutionModeNative  = "native"
	ExecutionModeHarness = "harness"

	ToolExecutionDockerAgent     = "docker-agent"
	ToolExecutionHarnessReported = "harness-reported"

	ToolApprovalDockerAgentPolicy = "docker-agent-policy"
	ToolApprovalExternal          = "external"
)

// ExecutionCapabilities describes an agent's execution integration, not enabled settings.
// Native support does not imply an adapter configured budgets or a policy requires approval.
type ExecutionCapabilities struct {
	Mode                       string `json:"mode"`
	ToolExecution              string `json:"tool_execution"`
	ToolApproval               string `json:"tool_approval"`
	BudgetSupported            bool   `json:"budget_supported"`
	CompactionSupported        bool   `json:"compaction_supported"`
	MidTurnSteeringSupported   bool   `json:"mid_turn_steering_supported"`
	PromptHookContextSupported bool   `json:"prompt_hook_context_supported"`
}

// AgentExecutionCapabilities inspects the named agent without starting tools or drivers.
// Unknown agents return nil; callers must not interpret absence as native support.
func (r *LocalRuntime) AgentExecutionCapabilities(name string) *ExecutionCapabilities {
	if r == nil || r.team == nil {
		return nil
	}
	a, err := r.team.Agent(name)
	if err != nil {
		return nil
	}
	return executionCapabilities(a)
}

func executionCapabilities(a *agent.Agent) *ExecutionCapabilities {
	if a == nil {
		return nil
	}
	if a.HasHarness() {
		return &ExecutionCapabilities{
			Mode:          ExecutionModeHarness,
			ToolExecution: ToolExecutionHarnessReported,
			ToolApproval:  ToolApprovalExternal,
		}
	}
	return &ExecutionCapabilities{
		Mode:                       ExecutionModeNative,
		ToolExecution:              ToolExecutionDockerAgent,
		ToolApproval:               ToolApprovalDockerAgentPolicy,
		BudgetSupported:            true,
		CompactionSupported:        true,
		MidTurnSteeringSupported:   true,
		PromptHookContextSupported: true,
	}
}
