package registry

import (
	"strings"
	"sync"
)

// CandidateModel is a model a credential offers before exclusions, aliases and prefixes.
type CandidateModel struct {
	ID          string
	DisplayName string
	Image       bool
}

var clientCandidates = struct {
	sync.RWMutex
	byClient map[string][]CandidateModel
}{byClient: map[string][]CandidateModel{}}

// RecordClientCandidates remembers the models a client offers before exclusions so the
// management API can list disabled models (which are not registered) and re-enable them.
func RecordClientCandidates(clientID string, models []*ModelInfo) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return
	}
	out := make([]CandidateModel, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		key := strings.ToLower(id)
		if id == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, CandidateModel{ID: id, DisplayName: model.DisplayName, Image: model.Type == OpenAIImageModelType})
	}
	clientCandidates.Lock()
	clientCandidates.byClient[clientID] = out
	clientCandidates.Unlock()
}

// ClientCandidates returns a copy of the models recorded for a client.
func ClientCandidates(clientID string) []CandidateModel {
	clientCandidates.RLock()
	defer clientCandidates.RUnlock()
	return append([]CandidateModel(nil), clientCandidates.byClient[strings.TrimSpace(clientID)]...)
}

// ForgetClientCandidates drops a client's recorded models.
func ForgetClientCandidates(clientID string) {
	clientCandidates.Lock()
	delete(clientCandidates.byClient, strings.TrimSpace(clientID))
	clientCandidates.Unlock()
}
