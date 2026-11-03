package cluster

import "testing"

func TestPostgresRebalanceGateMigratesPreviousSchema(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	// Simulate the preceding release: no independent mutex table and a v13
	// migration record. Startup must migrate before Coordinator.Initialize.
	if errDrop := repo.db.Migrator().DropTable(&ClusterRebalanceGateRecord{}); errDrop != nil {
		t.Fatal(errDrop)
	}
	if errUpdate := repo.db.Model(&schemaMigrationRecord{}).Where("id = ?", schemaMigrationRecordID).Update("version", 13).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errMigrate := AutoMigrate(repo.db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	if !repo.db.Migrator().HasTable(&ClusterRebalanceGateRecord{}) {
		t.Fatal("upgrade did not create the independent rebalance mutex")
	}
	assertPostgresSchemaMigrationVersion(t, repo.db, currentDatabaseVersion)
	models, ok := databaseSnapshotModels(13)
	if !ok {
		t.Fatal("previous snapshot format is no longer readable")
	}
	for _, model := range models {
		if model.name == "cluster_rebalance_gate" {
			t.Fatal("transient mutex leaked into the portable snapshot registry")
		}
	}
	f := seedRebalanceFixture(t, repo, 0)
	f.reconcile(t, 0)
}

func TestRebalanceGateRecoversMissingSingleton(t *testing.T) {
	for _, backend := range []struct {
		name string
		repo func(*testing.T) *Repository
	}{{"sqlite", newCredentialFoundationTestRepository}, {"postgres", newPostgresQuiescenceRepository}} {
		t.Run(backend.name, func(t *testing.T) {
			f := seedRebalanceFixture(t, backend.repo(t), 8)
			if errDelete := f.repo.db.Delete(&ClusterRebalanceGateRecord{}, "id = ?", 1).Error; errDelete != nil {
				t.Fatal(errDelete)
			}
			f.reconcile(t, 0)
			if len(f.plans(t)) != 2 {
				t.Fatal("recovered mutex lost ordinary reservations")
			}
		})
	}
}
