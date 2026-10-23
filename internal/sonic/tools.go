package sonic

import "github.com/mfbonfigli/dialout/internal/voice"

// callTools declares the agent's tools in Sonic's promptStart format.
func callTools(specs []voice.ToolSpec) []tool {
	var out []tool
	for _, t := range specs {
		out = append(out, tool{ToolSpec: toolSpec{Name: t.Name, Description: t.Description, InputSchema: inputSchema{JSON: t.Schema}}})
	}
	return out
}
