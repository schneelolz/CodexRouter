package history

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codexrouter/internal/api"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRestartRetainsRequestsPendingAndAuthoritativeProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Begin("POST /responses", "synthetic-thread")
	if err != nil {
		t.Fatal(err)
	}
	p := api.PendingSession{ID: "unknown-thread", FirstSeen: time.Now().UTC().Format(time.RFC3339Nano), Reason: "unknown_origin"}
	if err = s.PutPending(p); err != nil {
		t.Fatal(err)
	}
	bound := api.Binding{ID: "known-thread", ProviderID: "company", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = s.SyncBindings([]api.Binding{bound}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.Requests(20, "", "error")
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != id || page.Items[0].Result != "PROCESS_INTERRUPTED" {
		t.Fatalf("重启记录错误: %+v %v", page, err)
	}
	for range 2 {
		if err = s.SyncBindings([]api.Binding{bound}); err != nil {
			t.Fatal(err)
		}
	}
	known, _ := s.Sessions("bound", 20, "")
	pending, _ := s.Sessions("pending", 20, "")
	if known.Total != 1 || pending.Total != 1 {
		t.Fatal("幂等同步丢失或重复会话")
	}
	if err = s.Bind(api.Binding{ID: p.ID, ProviderID: "official", CreatedAt: bound.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.Sessions("pending", 20, "")
	if pending.Total != 0 {
		t.Fatal("登记后仍保留待登记")
	}
}

func TestIndexedPaginationSameTimestampSnapshotAndInvalidCursor(t *testing.T) {
	s := openTest(t)
	stamp := time.Now().UTC().UnixNano()
	for i := 0; i < 121; i++ {
		id, err := s.Begin("GET /models", "")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(id, 200, "HTTP_200", 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("UPDATE requests SET started=?", stamp); err != nil {
		t.Fatal(err)
	}
	first, err := s.Requests(20, "", "all")
	if err != nil || first.Total != 121 || len(first.Items) != 20 || first.NextCursor == "" {
		t.Fatalf("首屏 %+v %v", first, err)
	}
	if _, err = s.Begin("GET /models", ""); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	page := first
	for {
		for _, r := range page.Items {
			if seen[r.ID] {
				t.Fatal("分页重复")
			}
			seen[r.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		page, err = s.Requests(20, page.NextCursor, "all")
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 121 {
			t.Fatal("新插入污染原快照")
		}
	}
	if len(seen) != 121 {
		t.Fatalf("漏记录 %d", len(seen))
	}
	for _, raw := range []string{"bad", strings.Repeat("a", 1025), encode(cursor{Scope: "requests:error", Time: stamp, ID: "1", Max: 1})} {
		if _, err = s.Requests(20, raw, "all"); !errors.Is(err, ErrInvalidCursor) {
			t.Fatal("接受非法游标")
		}
	}
	rows, err := s.db.Query("EXPLAIN QUERY PLAN SELECT id FROM requests WHERE id<=? AND (started<? OR (started=? AND id<?)) ORDER BY started DESC,id DESC LIMIT 21", 121, stamp, stamp, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		indexed = indexed || strings.Contains(detail, "request_order")
	}
	if !indexed {
		t.Fatal("分页未使用排序索引")
	}
}

func TestRetentionBoundariesClearAndFailureRollback(t *testing.T) {
	s := openTest(t)
	now := time.Now().UTC()
	cutoff := now.Add(-24 * time.Hour)
	for _, stamp := range []time.Time{cutoff.Add(-time.Nanosecond), cutoff, cutoff.Add(time.Nanosecond)} {
		id, _ := s.Begin("POST /responses", "same-session")
		s.Finish(id, 200, "HTTP_200", 1)
		if _, err := s.db.Exec("UPDATE requests SET started=?,finished=? WHERE id=?", stamp.UnixNano(), now.Add(-time.Second).UnixNano(), id); err != nil {
			t.Fatal(err)
		}
	}
	active, _ := s.Begin("POST /responses", "same-session")
	if _, err := s.db.Exec("UPDATE requests SET started=? WHERE id=?", now.Add(-48*time.Hour).UnixNano(), active); err != nil {
		t.Fatal(err)
	}
	if days, err := s.Policy(); err != nil || days != 0 {
		t.Fatal("默认必须不限期")
	}
	n, err := s.SetPolicy(1, now)
	if err != nil || n != 1 {
		t.Fatalf("边界清理: %d %v", n, err)
	}
	if _, err = s.SetPolicy(3651, now); err == nil {
		t.Fatal("非法天数")
	}
	// 事务内删除被阻止时，策略变更也必须回滚。
	if _, err = s.db.Exec("CREATE TRIGGER reject_delete BEFORE DELETE ON requests BEGIN SELECT RAISE(ABORT,'synthetic-write-failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetPolicy(2, now.Add(5*24*time.Hour)); err == nil {
		t.Fatal("故障未报告")
	}
	days, _ := s.Policy()
	if days != 1 {
		t.Fatal("失败策略未回滚")
	}
	s.db.Exec("DROP TRIGGER reject_delete")
	// 操作开始后才结束的请求不能被本次清除删除。
	later, _ := s.Begin("POST /responses", "later")
	s.Finish(later, 200, "HTTP_200", 1)
	if n, err = s.Clear(now); err != nil || n != 2 {
		t.Fatalf("清除范围: %d %v", n, err)
	}
	page, _ := s.Requests(20, "", "all")
	if page.Total != 2 {
		t.Fatal("清除删了活动/稍后完成请求")
	}
}

func TestSessionPaginationAndConcurrentWritersCleanup(t *testing.T) {
	s := openTest(t)
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	bindings := []api.Binding{}
	for i := 0; i < 105; i++ {
		bindings = append(bindings, api.Binding{ID: fmt.Sprintf("session-%03d", i), ProviderID: "official", CreatedAt: stamp})
	}
	if err := s.SyncBindings(bindings); err != nil {
		t.Fatal(err)
	}
	page, _ := s.Sessions("bound", 20, "")
	count := len(page.Bindings)
	for page.NextCursor != "" {
		var err error
		page, err = s.Sessions("bound", 20, page.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		count += len(page.Bindings)
	}
	if count != 105 {
		t.Fatal("会话分页丢失")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				id, err := s.Begin("GET /models", "")
				if err == nil {
					err = s.Finish(id, 200, "HTTP_200", 1)
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 10 {
			if _, err := s.Clear(time.Now().UTC()); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	page, _ = s.Sessions("bound", 20, "")
	if page.Total != 105 {
		t.Fatal("请求清理影响会话")
	}
}

func TestCorruptAndUnknownVersionNeverRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	original := []byte("synthetic corrupt SQLite bytes")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("损坏未拒绝")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(original, after) {
		t.Fatal("覆盖了损坏数据库")
	}
	s := openTest(t)
	s.db.Exec("PRAGMA user_version=99")
	if err := s.initialize(); err == nil {
		t.Fatal("未知版本未拒绝")
	}
}

func TestLargeHistoryUsesBoundedPagesAndDoesNotCollectSecrets(t *testing.T) {
	s := openTest(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	statement, err := tx.Prepare("INSERT INTO requests(started,entry,thread,phase,result,status,finished) VALUES(?,'POST /responses',?,'finished','HTTP_200',200,?)")
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().UnixNano()
	for i := 0; i < 50000; i++ {
		if _, err = statement.Exec(stamp+int64(i), fmt.Sprintf("synthetic-%d", i), stamp+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	statement.Close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	page, err := s.Requests(100, "", "normal")
	if err != nil || len(page.Items) != 100 || page.Total != 50000 || page.NextCursor == "" {
		t.Fatalf("大量历史查询失败 %+v %v", len(page.Items), err)
	}
	second, err := s.Requests(100, page.NextCursor, "normal")
	if err != nil || second.Items[0].ID >= page.Items[len(page.Items)-1].ID {
		t.Fatal("大量历史排序错误")
	}
	// 表中只有显式允许的脱敏列，不通过正文解析补充计费或认证字段。
	rows, err := s.db.Query("PRAGMA table_info(requests)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"body", "authorization", "cookie", "key", "token"} {
			if strings.Contains(name, secret) {
				t.Fatalf("禁止的历史列: %s", name)
			}
		}
	}
}
