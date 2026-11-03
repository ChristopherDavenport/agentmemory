package agentmemory_test

import (
	"slices"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentmemory/storetest"
)

func TestMemStore(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentmemory.Store { return agentmemory.NewMemStore() },
	})
}

func TestMemStoreSmallBound(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentmemory.Store {
			return agentmemory.NewMemStore(agentmemory.WithMaxEntryBytes(256))
		},
	})
}

// TestStoresSearchWithOptions pins that the in-memory store supports
// every search option, so the conformance suite checks each of them,
// and that a store supporting none refuses each one (#47).
func TestStoresSearchWithOptions(t *testing.T) {
	all := []string{agentmemory.SearchAny, agentmemory.SearchMeta, agentmemory.SearchUpdatedAfter, agentmemory.SearchUpdatedBefore}
	if got := agentmemory.SearchSupport(agentmemory.NewMemStore()); !slices.Equal(got, all) {
		t.Errorf("SearchSupport(MemStore) = %v, want %v", got, all)
	}
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentmemory.Store { return plainStore{agentmemory.NewMemStore()} },
	})
}

// plainStore hides a store's optional interfaces.
type plainStore struct{ agentmemory.Store }
