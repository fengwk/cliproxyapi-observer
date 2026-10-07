package plugin

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-observer/internal/observer"
)

// Store is the observation store surface the plugin depends on. It mirrors the
// public *observer.Store API exactly; the concrete implementation is provided
// by the storage slice, which keeps the plugin testable with a faithful fake.
type Store interface {
	SubmitUsage(record pluginapi.UsageRecord) bool
	Capture(req pluginapi.RequestInterceptRequest) bool
	Flush(ctx context.Context) error
	Close() error
	Requests(query observer.Query) (observer.RequestPage, error)
	Summary(query observer.Query) (observer.Summary, error)
	Body(requestID string) (observer.BodyDetail, error)
	Status() observer.Status
}

// Opener parses raw plugin configuration and opens an observation store. Parse
// is kept separate from Open so a reconfigure can validate a candidate before
// the live store is quiesced and must not touch the database file.
type Opener interface {
	Parse(raw []byte) (observer.Config, error)
	Open(cfg observer.Config) (Store, error)
}

// AssetFunc resolves an embedded UI asset by name, returning its bytes, the
// response content type and whether it exists. Injection keeps the plugin
// package free of the frontend slice while main wires web.Asset in.
type AssetFunc func(name string) ([]byte, string, bool)
