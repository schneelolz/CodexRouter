package history

import (
	"database/sql"
	"path/filepath"
	"testing"

	"codexrouter/internal/api"
)

func TestObservationMigrationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := s.Begin("POST /responses", "synthetic-old")
	_ = s.Finish(id, 200, "REQUEST_CANCELED", 123)
	// 构造上版schema2，已有错误与指标在升级中不得重分类或重写。
	if _, err = s.db.Exec("ALTER TABLE requests DROP COLUMN observation; PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Requests(20, "", "error")
	if err != nil || page.Total != 1 || page.Items[0].Result != "REQUEST_CANCELED" || page.Items[0].Observation.MetricsStatus != "legacy" || len(page.Items[0].Metrics.KnownFields) != 0 {
		t.Fatalf("旧数据被伪造或重分类: %+v %v", page, err)
	}
	id, _ = s.Begin("POST /responses", "synthetic-new")
	want := api.RequestObservation{TerminalResult: "RESPONSE_COMPLETED", TransportResult: "REQUEST_CANCELED", MetricsStatus: "available"}
	if err = s.FinishActivity(api.RequestActivity{ID: id, Outcome: "success", Result: "RESPONSE_COMPLETED", Status: 200, Observation: want}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err = s.Requests(20, "", "success")
	if err != nil || page.Total != 1 || page.Items[0].Observation != want {
		t.Fatalf("观察事实重启丢失: %+v %v", page, err)
	}
}

func TestSchemaThreeMigrationFailurePreservesVersionTwo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := s.Begin("GET /models", "")
	_ = s.Finish(id, 200, "HTTP_200", 100)
	_, _ = s.db.Exec("PRAGMA user_version=2") // observation列冲突应拒绝迁移。
	s.Close()
	if s, err = Open(path); err == nil {
		s.Close()
		t.Fatal("冲突迁移未拒绝")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, count int
	_ = db.QueryRow("PRAGMA user_version").Scan(&version)
	_ = db.QueryRow("SELECT COUNT(*) FROM requests").Scan(&count)
	if version != 2 || count != 1 {
		t.Fatal("失败迁移破坏版本或记录")
	}
}
