package history

import (
	"testing"
	"time"
)

func TestNumberedSnapshotJumpFilterAndCleanup(t *testing.T) {
	s := openTest(t)
	for range 61 {
		id, _ := s.Begin("POST /responses", "test-numbered")
		if err := s.Finish(id, 200, "HTTP_200", 1); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.RequestNumberedPage(20, 1, "", "unconfirmed")
	if err != nil || first.Total != 61 || first.TotalPages != 4 || first.Page != 1 || first.Snapshot == "" {
		t.Fatalf("%+v %v", first, err)
	}
	newID, _ := s.Begin("POST /responses", "new-after-snapshot")
	_ = s.Finish(newID, 200, "HTTP_200", 1)
	last, err := s.RequestNumberedPage(20, 4, first.Snapshot, "unconfirmed")
	if err != nil || len(last.Items) != 1 || last.Total != 61 || last.Items[0].ID != first.Items[0].ID-60 {
		t.Fatalf("跳页或新插入污染 %+v %v", last, err)
	}
	for _, test := range []struct {
		limit, page      int
		snapshot, filter string
	}{{20, 0, "", "all"}, {20, 1000001, "", "all"}, {50, 1, first.Snapshot, "unconfirmed"}, {20, 1, first.Snapshot, "error"}, {20, 1, "bad", "all"}} {
		if _, err = s.RequestNumberedPage(test.limit, test.page, test.snapshot, test.filter); err == nil {
			t.Fatal("接受错误快照或页码")
		}
	}
	if _, err = s.Clear(time.Now()); err != nil {
		t.Fatal(err)
	}
	empty, err := s.RequestNumberedPage(20, 4, first.Snapshot, "unconfirmed")
	if err != nil || empty.Total != 0 || empty.Page != 1 || empty.TotalPages != 0 || len(empty.Items) != 0 {
		t.Fatalf("清理后页码未校准 %+v %v", empty, err)
	}
	// 空快照上界0仍应有效，后续新记录必须通过刷新才可见。
	id, _ := s.Begin("POST /responses", "new")
	_ = s.Finish(id, 200, "HTTP_200", 1)
	empty, err = s.RequestNumberedPage(20, 1, empty.Snapshot, "unconfirmed")
	if err != nil || empty.Total != 0 {
		t.Fatal("清空后的原快照吸入新数据")
	}
}
