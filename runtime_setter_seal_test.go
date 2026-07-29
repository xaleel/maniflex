package maniflex

import (
	"net/http"
	"testing"
)

type bug09DB struct {
	DBAdapter
	name string
}

type bug09Storage struct {
	FileStorage
	name string
}

type bug09KeyProvider struct {
	KeyProvider
	name string
}

func assertBug09Panics(t *testing.T, name string, call func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s after Handler was accepted", name)
		}
	}()
	call()
}

func TestRuntimeSettersRejectMutationAfterHandler(t *testing.T) {
	srv := bug03Server()
	db := &bug09DB{name: "initial"}
	storage := &bug09Storage{name: "initial"}
	keys := &bug09KeyProvider{name: "initial"}
	srv.SetDB(db)
	srv.SetStorage(storage)
	srv.SetKeyProvider(keys)

	_ = srv.Handler()

	assertBug09Panics(t, "SetDB", func() {
		srv.SetDB(&bug09DB{name: "late"})
	})
	assertBug09Panics(t, "SetStorage", func() {
		srv.SetStorage(&bug09Storage{name: "late"})
	})
	assertBug09Panics(t, "SetKeyProvider", func() {
		srv.SetKeyProvider(&bug09KeyProvider{name: "late"})
	})

	if srv.cfg.DB != db || srv.steps.adapter != db {
		t.Fatal("rejected SetDB changed the configured or active adapter")
	}
	if srv.cfg.FilesConfig.Storage != storage || srv.steps.storage != storage {
		t.Fatal("rejected SetStorage changed the configured or active storage")
	}
	if srv.cfg.KeyProvider != keys || srv.steps.keyProvider != keys {
		t.Fatal("rejected SetKeyProvider changed the configured or active provider")
	}
}

func TestRuntimeSetterAndHandlerRaceHasOneConsistentOutcome(t *testing.T) {
	cases := []struct {
		name       string
		set        func(*Server)
		configured func(*Server) bool
	}{
		{
			name: "SetDB",
			set: func(s *Server) {
				s.SetDB(&bug09DB{name: "race"})
			},
			configured: func(s *Server) bool {
				db, cfgOK := s.cfg.DB.(*bug09DB)
				active, stepOK := s.steps.adapter.(*bug09DB)
				return cfgOK && stepOK && db.name == "race" && active == db
			},
		},
		{
			name: "SetStorage",
			set: func(s *Server) {
				s.SetStorage(&bug09Storage{name: "race"})
			},
			configured: func(s *Server) bool {
				storage, cfgOK := s.cfg.FilesConfig.Storage.(*bug09Storage)
				active, stepOK := s.steps.storage.(*bug09Storage)
				return cfgOK && stepOK && storage.name == "race" && active == storage
			},
		},
		{
			name: "SetKeyProvider",
			set: func(s *Server) {
				s.SetKeyProvider(&bug09KeyProvider{name: "race"})
			},
			configured: func(s *Server) bool {
				keys, cfgOK := s.cfg.KeyProvider.(*bug09KeyProvider)
				active, stepOK := s.steps.keyProvider.(*bug09KeyProvider)
				return cfgOK && stepOK && keys.name == "race" && active == keys
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 64; i++ {
				srv := bug03Server()
				start := make(chan struct{})
				setterDone := make(chan any, 1)
				handlerDone := make(chan http.Handler, 1)

				go func() {
					<-start
					defer func() { setterDone <- recover() }()
					tc.set(srv)
				}()
				go func() {
					<-start
					handlerDone <- srv.Handler()
				}()
				close(start)

				panicValue := <-setterDone
				<-handlerDone
				configured := tc.configured(srv)
				switch {
				case panicValue == nil && !configured:
					t.Fatalf("iteration %d: successful setter did not update config and active step atomically", i)
				case panicValue != nil && configured:
					t.Fatalf("iteration %d: rejected setter partially changed sealed configuration", i)
				}
			}
		})
	}
}
