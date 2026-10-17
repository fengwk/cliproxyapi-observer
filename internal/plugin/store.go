package plugin

// AssetFunc resolves a public, secret-free embedded UI asset.
type AssetFunc func(string) ([]byte, string, bool)
