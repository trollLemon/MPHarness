package config

// Exported test helpers.
var NormalizeAllowedCommands = normalizeAllowedCommands
var ParseEnvBool = parseEnvBool
var CoreUtils = coreUtils

func Load(data []byte, sources Sources) (Config, error) {
	return load(data, sources)
}
