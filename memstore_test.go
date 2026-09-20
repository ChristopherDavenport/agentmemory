package agentmemory_test

import (
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
