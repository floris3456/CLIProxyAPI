package auth

// authCatalogLoad is immutable after publication. Prior entries remain visible
// during loading without waiting on store I/O or a plugin auth-parser callback.
type authCatalogLoad struct {
	state      string
	generation uint64
	prior      []*Auth
}

func cloneCatalog(auths []*Auth) []*Auth {
	out := make([]*Auth, 0, len(auths))
	for _, a := range auths {
		if a != nil {
			out = append(out, a.Clone())
		}
	}
	return out
}

// CatalogSnapshot returns runtime entries and their actual load state. It does
// not certify interval membership, admission authority or usage listener state.
// The startup gate prevents parser callbacks from waiting on Load's store I/O,
// without substituting an empty catalog when unrelated runtime mutation is busy.
func (m *Manager) CatalogSnapshot() ([]*Auth, string, uint64) {
	m.catalogStartMu.RLock()
	defer m.catalogStartMu.RUnlock()
	load := m.catalogLoad.Load()
	if load != nil && load.state == "loading" {
		return cloneCatalog(load.prior), load.state, load.generation
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	load = m.catalogLoad.Load()
	state, generation := "uninitialized", uint64(0)
	if load != nil {
		state, generation = load.state, load.generation
	}
	entries := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		if a != nil {
			entries = append(entries, a.Clone())
		}
	}
	return entries, state, generation
}
