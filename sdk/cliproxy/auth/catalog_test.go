package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

type catalogStore struct {
	items []*Auth
	err   error
}

type loadingCatalogObservation struct {
	items      []*Auth
	state      string
	generation uint64
}

type loadingCatalogStore struct {
	*catalogStore
	manager *Manager
	entered chan loadingCatalogObservation
	release chan struct{}
}

func (s *loadingCatalogStore) List(ctx context.Context) ([]*Auth, error) {
	// The real store can invoke a plugin parser which calls back into the host
	// catalog. Exercise that reentrant boundary while Load owns the auth lock.
	items, state, generation := s.manager.CatalogSnapshot()
	select {
	case s.entered <- loadingCatalogObservation{items, state, generation}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.catalogStore.List(ctx)
}

func TestPacerStartupConcurrentLoadsKeepCatalogVisible(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &loadingCatalogStore{catalogStore: &catalogStore{items: []*Auth{{ID: "runtime-a", Provider: "claude"}}}, entered: make(chan loadingCatalogObservation, 2), release: make(chan struct{})}
	m := NewManager(store, nil, nil)
	store.manager = m
	a, err := m.Register(ctx, &Auth{ID: "runtime-a", Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- m.Load(ctx) }()
	waitLoad := func(want uint64) {
		t.Helper()
		select {
		case got := <-store.entered:
			if got.state != "loading" || got.generation != want || len(got.items) != 1 || got.items[0].ID != a.ID || got.items[0].Index != a.Index {
				t.Fatalf("parser callback lost prior runtime identity: %+v", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("auth parser catalog callback deadlocked")
		}
	}
	waitLoad(1)
	secondStarted := make(chan struct{})
	go func() { close(secondStarted); done <- m.Load(ctx) }()
	<-secondStarted
	read := make(chan loadingCatalogObservation, 1)
	go func() {
		items, state, generation := m.CatalogSnapshot()
		read <- loadingCatalogObservation{items, state, generation}
	}()
	select {
	case got := <-read:
		if got.state != "loading" || len(got.items) != 1 || got.items[0].Index != a.Index {
			t.Fatal("queued reload hid current catalog", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued reload blocked catalog behind store I/O")
	}
	store.release <- struct{}{}
	waitLoad(2)
	store.release <- struct{}{}
	for range 2 {
		select {
		case err = <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("reload did not complete")
		}
	}
	items, state, generation := m.CatalogSnapshot()
	if state != "ready" || generation != 2 || len(items) != 1 || items[0].Index != a.Index {
		t.Fatal("completed reload changed immutable index", state, generation)
	}
}

func (s *catalogStore) List(context.Context) ([]*Auth, error)       { return s.items, s.err }
func (s *catalogStore) Save(context.Context, *Auth) (string, error) { return "", nil }
func (s *catalogStore) Delete(context.Context, string) error        { return nil }

func TestPacerStartupCatalogFailureDoesNotHideRegisteredEntries(t *testing.T) {
	ctx := context.Background()
	store := &catalogStore{err: errors.New("dummy load failure")}
	m := NewManager(store, nil, nil)
	if _, err := m.Register(ctx, &Auth{ID: "registered-before-load", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	items, state, generation := m.CatalogSnapshot()
	if state != "uninitialized" || generation != 0 || len(items) != 1 || items[0].Index == "" {
		t.Fatal("Register fabricated readiness or lost runtime identity", state, generation)
	}
	if err := m.Load(ctx); err == nil {
		t.Fatal("load failure lost")
	}
	if _, err := m.Register(ctx, &Auth{ID: "config-after-failure", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	items, state, generation = m.CatalogSnapshot()
	if state != "failed" || generation != 1 || len(items) != 2 {
		t.Fatal("failed load hid subsequent runtime registration", state, generation, len(items))
	}
	items[0].ID = "modified-clone"
	if _, ok := m.GetByID("modified-clone"); ok {
		t.Fatal("catalog mutated runtime identity")
	}
	store.err = nil
	store.items = []*Auth{{ID: "loaded-actual-store", Provider: "claude"}}
	if err := m.Load(ctx); err != nil {
		t.Fatal(err)
	}
	items, state, generation = m.CatalogSnapshot()
	if state != "ready" || generation != 2 || len(items) != 1 || items[0].ID != "loaded-actual-store" || items[0].Index == "" {
		t.Fatal("actual load did not establish runtime catalog", state, generation)
	}
}
