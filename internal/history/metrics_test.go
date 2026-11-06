package history

import (
	"database/sql"
	"path/filepath"
	"testing"

	"codexrouter/internal/api"
)

func TestSemanticFiltersAndMetricsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"success", "error", "unconfirmed"} {
		id, err := s.Begin("POST /responses", "synthetic-session")
		if err != nil {
			t.Fatal(err)
		}
		err = s.FinishActivity(api.RequestActivity{ID: id, Status: 200, Outcome: outcome, Result: map[string]string{"success": "RESPONSE_COMPLETED", "error": "RESPONSE_FAILED", "unconfirmed": "HTTP_200"}[outcome], Model: "synthetic-model", RequestType: "stream", Metrics: api.RequestMetrics{InputTokens: 100, OutputTokens: 10, TotalTokens: 110, CacheReadTokens: 90, KnownFields: []string{"input_tokens", "output_tokens", "total_tokens", "cache_read_tokens"}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.Begin("POST /responses", "active")
	if err != nil {
		t.Fatal(err)
	}
	for _, filter := range []string{"success", "error", "unconfirmed", "processing"} {
		page, err := s.Requests(20, "", filter)
		if err != nil || page.Total != 1 {
			t.Fatalf("%s %+v %v", filter, page, err)
		}
		if filter != "processing" && (page.Items[0].Metrics.TotalTokens != 110 || page.Items[0].Model != "synthetic-model") {
			t.Fatal("指标丢失")
		}
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, _ := s.Requests(20, "", "success")
	if page.Total != 1 || page.Items[0].Metrics.CacheReadTokens != 90 {
		t.Fatal("重启丢失用量")
	}
	page, _ = s.Requests(20, "", "error")
	if page.Total != 2 {
		t.Fatal("活动请求重启后未归入错误")
	}
}

func legacyDatabase(t *testing.T, path string, conflict bool) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE requests(id INTEGER PRIMARY KEY AUTOINCREMENT, started INTEGER NOT NULL, entry TEXT NOT NULL, thread TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '', phase TEXT NOT NULL, result TEXT NOT NULL, status INTEGER NOT NULL DEFAULT 0, elapsed INTEGER NOT NULL DEFAULT 0, finished INTEGER NOT NULL DEFAULT 0);
CREATE TABLE sessions(id TEXT PRIMARY KEY, kind TEXT NOT NULL, provider TEXT NOT NULL, stamp INTEGER NOT NULL, reason TEXT NOT NULL);
CREATE TABLE policy(id INTEGER PRIMARY KEY, days INTEGER NOT NULL);
INSERT INTO policy VALUES(1,14);
INSERT INTO requests(started,entry,thread,phase,result,status,finished) VALUES(100,'POST /responses','old-success','finished','HTTP_200',200,101),(102,'POST /responses','old-error','finished','HTTP_502',502,103),(104,'POST /responses','active','received','处理中',0,0);
INSERT INTO sessions VALUES('bound','bound','official',100,'');
PRAGMA user_version=1;`)
	if err != nil {
		t.Fatal(err)
	}
	if conflict {
		if _, err = db.Exec("ALTER TABLE requests ADD COLUMN metrics TEXT"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyMigrationPreservesUnknownUsageAndBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	legacyDatabase(t, path, false)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.Requests(20, "", "unconfirmed")
	if err != nil || page.Total != 1 || page.Items[0].ThreadID != "old-success" || len(page.Items[0].Metrics.KnownFields) != 0 {
		t.Fatalf("旧200伪装成功: %+v %v", page, err)
	}
	page, _ = s.Requests(20, "", "success")
	if page.Total != 0 {
		t.Fatal("旧记录冒充成功")
	}
	page, _ = s.Requests(20, "", "error")
	if page.Total != 2 {
		t.Fatal("旧错误或活动记录丢失")
	}
	sessions, _ := s.Sessions("bound", 20, "")
	days, _ := s.Policy()
	if sessions.Total != 1 || days != 14 {
		t.Fatal("升级影响绑定或保留策略")
	}
}

func TestMigrationFailureRollsBackEveryAddedColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	legacyDatabase(t, path, true)
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("冲突库未拒绝")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, count int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM requests").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if version != 1 || count != 3 {
		t.Fatal("失败迁移改变原版本/记录")
	}
	if _, err = db.Exec("SELECT outcome FROM requests"); err == nil {
		t.Fatal("迁移前半部分没有回滚")
	}
}
