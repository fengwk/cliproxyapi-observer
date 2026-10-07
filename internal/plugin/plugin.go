// Package plugin implements the CLIProxyAPI native observer plugin: usage and
// request-interceptor observation plus a read-only management API. It never
// changes inference behavior.
package plugin

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// PluginID is the host-local plugin identifier and the management route
// prefix. The host derives it from the plugin library filename, not from the
// display metadata below.
const PluginID = "cliproxyapi-observer"

// PluginName is the human-readable plugin name shown in the host store and
// management UI. The technical PluginID stays visible for support.
const PluginName = "Observer"

// Version is the plugin release version. It can be overridden at build time via
// -ldflags "-X github.com/fengwk/cliproxyapi-observer/internal/plugin.Version=...".
var Version = "0.1.0"

// GitHubRepository satisfies the host registration metadata requirement.
const GitHubRepository = "https://github.com/fengwk/cliproxyapi-observer"

// Metadata returns the registration metadata reported to the host.
func Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:             PluginName,
		Version:          Version,
		Author:           "fengwk",
		GitHubRepository: GitHubRepository,
		ConfigFields: []pluginapi.ConfigField{
			{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable local observation, storage and management endpoints."},
			{Name: "db", Type: pluginapi.ConfigFieldTypeString, Description: "bbolt database path; relative paths resolve against the CPA working directory (default data/cliproxyapi-observer.db)."},
			{Name: "stats-retention-days", Type: pluginapi.ConfigFieldTypeInteger, Description: "Retention of aggregated statistics in days (default 365)."},
			{Name: "request-retention", Type: pluginapi.ConfigFieldTypeString, Description: "Retention of request metadata, e.g. 24h (default 24h)."},
			{Name: "body-retention", Type: pluginapi.ConfigFieldTypeString, Description: "Retention of captured request bodies, never more than 24h (default 24h)."},
			{Name: "capture-bodies", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Opt in to storing sanitized request bodies (default false)."},
			{Name: "max-body-bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Largest request body stored per request in bytes (default 1048576)."},
			{Name: "max-body-storage-bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Total captured body storage budget in bytes (default 268435456)."},
			{Name: "flush", Type: pluginapi.ConfigFieldTypeString, Description: "Async writer batch interval, e.g. 1s (default 1s)."},
			{Name: "prices", Type: pluginapi.ConfigFieldTypeObject, Description: "Exact full model id to USD-per-million-token price map (input, output, cache-read, cache-creation)."},
		},
	}
}

// registration is the plugin.register / plugin.reconfigure response payload.
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

// registrationCapability declares exactly the three capabilities this observer
// uses. It deliberately omits every executor, router and response/stream hook
// so the host can never route inference through the plugin.
type registrationCapability struct {
	UsagePlugin        bool `json:"usage_plugin"`
	RequestInterceptor bool `json:"request_interceptor"`
	ManagementAPI      bool `json:"management_api"`
}

// registrationResponse negotiates the schema conservatively: never claim a
// newer contract than the host offered, and never exceed the SDK schema.
func registrationResponse(hostSchema uint32) registration {
	schema := pluginabi.SchemaVersion
	if hostSchema != 0 && hostSchema < schema {
		schema = hostSchema
	}
	if schema == 0 {
		schema = 1
	}
	return registration{
		SchemaVersion: schema,
		Metadata:      Metadata(),
		Capabilities: registrationCapability{
			UsagePlugin:        true,
			RequestInterceptor: true,
			ManagementAPI:      true,
		},
	}
}
