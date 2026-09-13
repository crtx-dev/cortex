// Package operations declares Cortex's canonical functional operation surface.
package operations

import (
	"github.com/gantry-tools/gantry-core/contracttest"
	"github.com/gantry-tools/gantry-core/operation"
)

var Contracts = []operation.Contract{
	contract("cortex.status.read", operation.Read, "GET", "/api/status", "status", "get", operation.Session, ""),
	contract("cortex.settings.update", operation.Mutation, "POST", "/api/settings", "settings", "update", operation.Session, "cortex.settings.updated"),
	contract("cortex.totp.disable", operation.Destructive, "POST", "/api/auth/totp/disable", "totp", "disable", operation.Session, "cortex.totp.disabled"),
}

func contract(id string, kind operation.Kind, method, path, resource, verb string, boundary operation.Boundary, event string) operation.Contract {
	audit := operation.Audit{}
	if kind != operation.Read {
		audit = operation.Audit{Required: true, Event: event}
	}
	return operation.Contract{SchemaVersion: operation.SchemaVersion, ID: id, Kind: kind, Route: operation.Route{Method: method, Path: path}, CLI: &operation.CLI{Resource: resource, Verb: verb}, Authorization: operation.Authorization{Boundary: boundary}, Audit: audit, Idempotency: operation.Idempotency{RetrySafe: kind == operation.Read}, Automation: operation.Automatable}
}

func AdoptionManifest() contracttest.Manifest {
	routes := make([]operation.Route, len(Contracts))
	ids := make([]string, len(Contracts))
	for i, c := range Contracts {
		routes[i], ids[i] = c.Route, c.ID
	}
	return contracttest.Manifest{SchemaVersion: 1, Project: "cortex", Operations: Contracts, ObservedRoutes: routes, WebsiteOperations: ids}
}
