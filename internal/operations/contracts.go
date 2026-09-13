// Package operations declares Cortex's canonical functional operation surface.
package operations

import (
	"github.com/crtx-dev/cortex/internal/app"
	"github.com/gantry-tools/gantry-core/contracttest"
	"github.com/gantry-tools/gantry-core/operation"
)

type spec struct {
	id, method, path, resource, verb string
	kind                             operation.Kind
	automation                       operation.Automation
	website                          bool
	test                             string
}

var specs = []spec{
	{"cortex.launcher.instances.list", "GET", "/api/launcher/instances", "launcher-instances", "list", operation.Read, operation.Automatable, true, "internal/app/launcher_test.go"},
	{"cortex.launcher.config.update", "PUT", "/api/launcher/config", "launcher-config", "update", operation.Mutation, operation.Automatable, true, "internal/app/launcher_test.go"},
	{"cortex.auth.state", "GET", "/api/auth/state", "auth", "state", operation.Read, operation.Automatable, true, "internal/app/auth_hardening_test.go"},
	{"cortex.auth.setup", "POST", "/api/auth/setup", "auth", "setup", operation.Mutation, operation.Automatable, true, "internal/app/auth_hardening_test.go"},
	{"cortex.auth.login", "POST", "/api/auth/login", "auth", "login", operation.Mutation, operation.Automatable, true, "internal/app/auth_hardening_test.go"},
	{"cortex.auth.logout", "POST", "/api/auth/logout", "auth", "logout", operation.Destructive, operation.Automatable, true, "internal/app/auth_hardening_test.go"},
	{"cortex.auth.password.update", "POST", "/api/auth/password", "auth-password", "update", operation.Mutation, operation.Automatable, true, "internal/app/auth_hardening_test.go"},
	{"cortex.auth.totp.begin", "POST", "/api/auth/totp/begin", "totp", "begin", operation.Mutation, operation.Automatable, true, "internal/app/auth_hardening_test.go"},
	{"cortex.auth.totp.enable", "POST", "/api/auth/totp/enable", "totp", "enable", operation.Mutation, operation.Automatable, true, "internal/app/auth_hardening_test.go"},
	{"cortex.auth.totp.disable", "POST", "/api/auth/totp/disable", "totp", "disable", operation.Destructive, operation.Automatable, true, "internal/app/auth_hardening_test.go"},
	{"cortex.auth.google.update", "POST", "/api/auth/google", "google-auth", "update", operation.Mutation, operation.Automatable, true, "internal/app/auth_hardening_test.go"},
	{"cortex.users.list", "GET", "/api/manage/users", "users", "list", operation.Read, operation.Automatable, true, "internal/app/accounts_test.go"},
	{"cortex.users.manage", "POST", "/api/manage/users", "users", "manage", operation.Mutation, operation.Automatable, true, "internal/app/accounts_test.go"},
	{"cortex.roles.list", "GET", "/api/manage/roles", "roles", "list", operation.Read, operation.Automatable, true, "internal/app/accounts_test.go"},
	{"cortex.oauth.google.start", "GET", "/api/auth/google/start", "", "", operation.Read, operation.BrowserProtocol, true, "internal/app/auth_hardening_test.go"},
	{"cortex.oauth.google.callback", "GET", "/api/auth/google/callback", "", "", operation.Read, operation.BrowserProtocol, false, "internal/app/auth_hardening_test.go"},
	{"cortex.status.read", "GET", "/api/status", "status", "get", operation.Read, operation.Automatable, true, "internal/app/app_test.go"},
	{"cortex.health.read", "GET", "/api/health", "health", "get", operation.Read, operation.Automatable, false, "internal/app/routes_test.go"},
	{"cortex.settings.read", "GET", "/api/settings", "settings", "get", operation.Read, operation.Automatable, true, "internal/app/app_test.go"},
	{"cortex.settings.update", "POST", "/api/settings", "settings", "update", operation.Mutation, operation.Automatable, true, "internal/app/app_test.go"},
	{"cortex.files.list", "GET", "/api/files", "files", "list", operation.Read, operation.Automatable, true, "internal/app/workspace_hardening_test.go"},
	{"cortex.file.read", "GET", "/api/file", "file", "get", operation.Read, operation.Automatable, true, "internal/app/workspace_hardening_test.go"},
	{"cortex.agent.status", "GET", "/api/agent/status", "agent", "status", operation.Read, operation.Automatable, true, "internal/app/agent_lifecycle_test.go"},
	{"cortex.agent.run", "POST", "/api/agent/run", "agent", "run", operation.Mutation, operation.StreamingProtocol, true, "internal/app/agent_outcome_test.go"},
	{"cortex.agent.cancel", "POST", "/api/agent/cancel", "agent", "cancel", operation.Mutation, operation.Automatable, true, "internal/app/agent_outcome_test.go"},
	{"cortex.agent.diagnostics", "GET", "/api/agent/run-diagnostics", "agent", "diagnostics", operation.Read, operation.Automatable, true, "internal/app/agent_outcome_test.go"},
	{"cortex.agent.image", "GET", "/api/agent/image", "agent", "image", operation.Read, operation.Automatable, true, "internal/app/app_test.go"},
	{"cortex.agent.models", "GET", "/api/agent/models", "agent", "models", operation.Read, operation.Automatable, true, "internal/app/app_test.go"},
	{"cortex.conversations.list", "GET", "/api/conversations", "conversations", "list", operation.Read, operation.Automatable, true, "internal/app/conversations_test.go"},
	{"cortex.conversation.update", "PUT", "/api/conversation", "conversation", "update", operation.Mutation, operation.Automatable, true, "internal/app/conversation_integrity_test.go"},
	{"cortex.conversation.delete", "DELETE", "/api/conversation", "conversation", "delete", operation.Destructive, operation.Automatable, true, "internal/app/conversation_integrity_test.go"},
}

var Contracts = buildContracts()

func buildContracts() []operation.Contract {
	inventory := map[string]app.OperationRoute{}
	for _, r := range app.OperationRouteInventory() {
		inventory[r.Method+" "+r.Path] = r
	}
	out := make([]operation.Contract, 0, len(specs))
	for _, s := range specs {
		r := inventory[s.method+" "+s.path]
		boundary := operation.Session
		if r.Boundary == "public" {
			boundary = operation.Public
		}
		var cli *operation.CLI
		if s.resource != "" {
			cli = &operation.CLI{Resource: s.resource, Verb: s.verb, Implemented: true}
		}
		audit := operation.Audit{}
		if s.kind != operation.Read {
			audit = operation.Audit{Required: true, Event: s.id + ".performed"}
		}
		schemas := operation.Schemas{Output: s.id + ".response.v1"}
		if s.kind != operation.Read {
			schemas.Input = s.id + ".request.v1"
		}
		out = append(out, operation.Contract{SchemaVersion: operation.SchemaVersion, ID: s.id, Kind: s.kind, Route: operation.Route{Method: s.method, Path: s.path}, CLI: cli, Authorization: operation.Authorization{Boundary: boundary}, Schemas: schemas, Audit: audit, Idempotency: operation.Idempotency{RetrySafe: s.kind == operation.Read}, Automation: s.automation})
	}
	return out
}

func Manifest() contracttest.Manifest {
	routes := make([]operation.Route, 0)
	for _, r := range app.OperationRouteInventory() {
		routes = append(routes, operation.Route{Method: r.Method, Path: r.Path})
	}
	commands := make([]operation.CLI, 0)
	website := make([]string, 0)
	evidence := map[string]contracttest.Evidence{}
	for i, c := range Contracts {
		if c.CLI != nil && c.CLI.Implemented {
			commands = append(commands, *c.CLI)
		}
		if specs[i].website {
			website = append(website, c.ID)
		}
		evidence[c.ID] = contracttest.Evidence{Website: specs[i].website, Tests: []string{specs[i].test}}
	}
	return contracttest.Manifest{SchemaVersion: 1, Project: "cortex", Operations: Contracts, ObservedRoutes: routes, ObservedCommands: commands, WebsiteOperations: website, Evidence: evidence}
}

func AdoptionManifest() contracttest.Manifest { return Manifest() }
