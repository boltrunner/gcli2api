package gemini

// ModelInfo describes a supported model for discovery and validation.
type ModelInfo struct {
	Name        string
	DisplayName string
	Description string
}

// SupportedModels is the canonical list of supported model identifiers.
var SupportedModels = []ModelInfo{
	{Name: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash", Description: "Fast multimodal generation"},
	{Name: "gemini-2.5-flash-lite", DisplayName: "Gemini 2.5 Flash Lite", Description: "Fast multimodal generation"},
	{Name: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro", Description: "Accurate multimodal generation"},
}

// IsSupportedModel reports whether the given model name is supported.
func IsSupportedModel(name string) bool {
	for _, m := range SupportedModels {
		if m.Name == name {
			return true
		}
	}
	return false
}
