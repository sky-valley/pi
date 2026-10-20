package coding

import "github.com/sky-valley/pi/agent"

// BashSpawnContext is pi's BashSpawnContext (bash.ts): what a command is
// about to run as. Env is the child environment in os/exec's KEY=value form.
type BashSpawnContext struct {
	Command string
	Cwd     string
	Env     []string
}

// BashSpawnHook is pi's BashSpawnHook: it adjusts the command, cwd or env
// before execution, and what it returns is what runs.
type BashSpawnHook func(BashSpawnContext) BashSpawnContext

// BashToolOptions is pi's BashToolOptions (bash.ts).
type BashToolOptions struct {
	// Operations executes commands; nil is the local shell
	// (LocalBashOperations over ShellPath).
	Operations *BashOperations
	// CommandPrefix is prepended to every command on its own line, for
	// example shell setup commands.
	CommandPrefix string
	// ShellPath is an explicit shell path, as from pi's shellPath setting.
	ShellPath string
	// ExposeSessionEnvironment exposes the session's metadata as PI_*
	// environment variables and advertises them in the prompt guidelines.
	// nil is pi's default, true.
	ExposeSessionEnvironment *bool
	// SpawnHook adjusts command, cwd or env before execution.
	SpawnHook BashSpawnHook
}

// The PowerShell tool shares the bash tool's seams, as pi's powershell.ts
// aliases them.
type (
	PowerShellOperations   = BashOperations
	PowerShellSpawnContext = BashSpawnContext
	PowerShellSpawnHook    = BashSpawnHook
)

// PowerShellToolOptions is pi's PowerShellToolOptions: the operations,
// exposeSessionEnvironment and spawnHook of BashToolOptions.
type PowerShellToolOptions struct {
	// Operations executes commands; nil is LocalPowerShellOperations.
	Operations               *PowerShellOperations
	ExposeSessionEnvironment *bool
	SpawnHook                PowerShellSpawnHook
}

// CreateBashTool ports pi's createBashTool(cwd, options): the bash tool
// rooted at cwd. A nil options is pi's omitted options. The tool exposes no
// PI_* session values: there is no session to read them from, matching pi's
// `exposeSessionEnvironment && ctx` guard for a tool run without an extension
// context.
func CreateBashTool(cwd string, options *BashToolOptions) agent.AgentTool {
	return createShellTool(cwd, bashToolConfig, options, nil)
}

// CreatePowerShellTool ports pi's createPowerShellTool(cwd, options).
func CreatePowerShellTool(cwd string, options *PowerShellToolOptions) agent.AgentTool {
	return createPowerShellTool(cwd, options, nil)
}

// createPowerShellTool ports createPowerShellToolDefinition: the shared shell
// tool with the PowerShell config and, unless the caller supplies its own,
// the local PowerShell operations.
func createPowerShellTool(cwd string, options *PowerShellToolOptions, sessionEnv sessionEnvFn) agent.AgentTool {
	if options == nil {
		options = &PowerShellToolOptions{}
	}
	operations := options.Operations
	if operations == nil {
		local := LocalPowerShellOperations()
		operations = &local
	}
	return createShellTool(cwd, powershellToolConfig, &BashToolOptions{
		Operations:               operations,
		ExposeSessionEnvironment: options.ExposeSessionEnvironment,
		SpawnHook:                options.SpawnHook,
	}, sessionEnv)
}

// resolveSpawnContext ports pi's resolveSpawnContext (bash.ts): the inherited
// environment minus the PI_* session keys, plus the session's values when
// they are exposed and there is a session, then the spawn hook.
func resolveSpawnContext(command, cwd string, spawnHook BashSpawnHook, exposeSessionEnvironment bool, sessionEnv sessionEnvFn) BashSpawnContext {
	if !exposeSessionEnvironment {
		sessionEnv = nil
	}
	base := BashSpawnContext{Command: command, Cwd: cwd, Env: bashCommandEnv(sessionEnv)}
	if spawnHook != nil {
		return spawnHook(base)
	}
	return base
}
