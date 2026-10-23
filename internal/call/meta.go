package call

import "encoding/json"

// finalStatuses are the statuses of a call that is over.
var finalStatuses = map[string]bool{
	"completed": true, "busy": true, "failed": true, "no-answer": true, "canceled": true, "interrupted": true,
}

// MarkInterrupted is the fix for recorder.Recover: a call recovered from the spool whose
// meta.json doesn't show a final status was cut by a crash or a hibernation.
func MarkInterrupted(meta []byte) []byte {
	var m map[string]any
	if json.Unmarshal(meta, &m) != nil || m == nil {
		m = map[string]any{}
	}
	if s, _ := m["status"].(string); finalStatuses[s] {
		return meta
	}
	m["status"] = "interrupted"
	b, _ := json.Marshal(m)
	return b
}
