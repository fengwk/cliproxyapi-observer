// Package plugin implements client Key authentication and admission without
// taking over the host's other authentication providers.
package plugin

import "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

const PluginID = "cliproxyapi-observer"
const PluginName = "Observer"
const GitHubRepository = "https://github.com/fengwk/cliproxyapi-observer"
const AuthIdentifier = "observer"
const AccessProvider = "plugin:" + PluginID + ":" + AuthIdentifier

var Version = "1.0.3"

func Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{Name: PluginName, Version: Version, Author: "fengwk", GitHubRepository: GitHubRepository, ConfigFields: []pluginapi.ConfigField{
		{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable client Key authentication and accounting (default true)."},
		{Name: "db", Type: pluginapi.ConfigFieldTypeString, Description: "Database path (default data/cliproxyapi-observer-v2.db)."},
	}}
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]bool    `json:"capabilities"`
}

func registrationResponse() registration {
	// FrontendAuthProviderExclusive is intentionally absent: native CPA Keys and
	// other plugins must stay in the host authentication chain.
	return registration{6, Metadata(), map[string]bool{"frontend_auth_provider": true, "request_interceptor": true, "request_lifecycle_plugin": true, "usage_plugin": true, "management_api": true}}
}
