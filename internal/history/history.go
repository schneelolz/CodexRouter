// Package history 提供与路由权威绑定隔离的 SQLite 展示历史。
package history

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"codexrouter/internal/api"
	_ "modernc.org/sqlite"
)

// ErrInvalidCursor 表示游标损坏、类型或筛选条件不匹配。
var ErrInvalidCursor = errors.New("无效的分页游标")

// Store 串行化短事务；数据库不包含认证、正文或路由权威数据。
type Store struct {
	mu sync.Mutex // 保护生命周期及查询与清理的一致性。
	db *sql.DB
}

type cursor struct {
	Scope string `json:"scope"`
	Time  int64  `json:"time"`
	ID    string `json:"id"`
	Max   int64  `json:"max"`
}

func encode(c cursor) string { b, _ := json.Marshal(c); return base64.RawURLEncoding.EncodeToString(b) }
func decode(raw, scope string) (cursor, error) {
	var c cursor
	if len(raw) > 1024 {
		return c, ErrInvalidCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(b, &c) != nil || c.Scope != scope || c.Time <= 0 || c.ID == "" || len(c.ID) > 128 || c.Max < 0 {
		return c, ErrInvalidCursor
	}
	return c, nil
}

// Open 打开版本化历史库；损坏或未知版本拒绝打开，不删除原文件。
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
			return nil, errors.New("历史数据库必须是普通文件")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if info, err := os.Lstat(path + suffix); err == nil && !info.Mode().IsRegular() {
				return nil, errors.New("历史数据库附属文件必须是普通文件")
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err = s.initialize(); err != nil {
		db.Close()
		return nil, fmt.Errorf("历史数据库不可用: %w", err)
	}
	return s, nil
}

func (s *Store) initialize() error {
	if _, err := s.db.Exec("PRAGMA busy_timeout=1500"); err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != 1 && version != 2 && version != 3 {
		return errors.New("历史数据库版本不兼容")
	}
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA wal_autocheckpoint=256; PRAGMA journal_size_limit=8388608"); err != nil {
		return err
	}
	if version == 0 {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		_, err = tx.Exec(`CREATE TABLE requests(id INTEGER PRIMARY KEY AUTOINCREMENT, started INTEGER NOT NULL, entry TEXT NOT NULL, thread TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '', phase TEXT NOT NULL, result TEXT NOT NULL, status INTEGER NOT NULL DEFAULT 0, elapsed INTEGER NOT NULL DEFAULT 0, finished INTEGER NOT NULL DEFAULT 0);
CREATE INDEX request_order ON requests(started DESC,id DESC);
CREATE INDEX request_phase_order ON requests(phase,started DESC,id DESC);
CREATE TABLE sessions(id TEXT PRIMARY KEY, kind TEXT NOT NULL, provider TEXT NOT NULL, stamp INTEGER NOT NULL, reason TEXT NOT NULL);
CREATE INDEX session_order ON sessions(kind,stamp DESC,id DESC);
CREATE TABLE policy(id INTEGER PRIMARY KEY CHECK(id=1), days INTEGER NOT NULL CHECK(days BETWEEN 0 AND 3650));
INSERT INTO policy VALUES(1,0); PRAGMA user_version=1;`)
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	if version < 2 {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		_, err = tx.Exec(`ALTER TABLE requests ADD COLUMN outcome TEXT NOT NULL DEFAULT 'unconfirmed';
ALTER TABLE requests ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN request_type TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE requests ADD COLUMN error_stage TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN metrics TEXT NOT NULL DEFAULT '{"known_fields":[]}';
UPDATE requests SET outcome=CASE WHEN phase='received' THEN 'processing' WHEN NOT(status BETWEEN 200 AND 299 AND result LIKE 'HTTP_%') THEN 'error' ELSE 'unconfirmed' END;
UPDATE requests SET request_type='auxiliary' WHERE entry='GET /models';
CREATE INDEX request_outcome_order ON requests(outcome,started DESC,id DESC);
PRAGMA user_version=2;`)
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	if version < 3 {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`ALTER TABLE requests ADD COLUMN observation TEXT NOT NULL DEFAULT '{}'; PRAGMA user_version=3;`); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	_, err := s.db.Exec("UPDATE requests SET phase='finished',outcome='error',error_stage='process',result='PROCESS_INTERRUPTED',finished=? WHERE phase='received'", time.Now().UTC().UnixNano())
	return err
}

// Close 释放数据库连接；不删除数据库。
func (s *Store) Close() error { s.mu.Lock(); defer s.mu.Unlock(); return s.db.Close() }

// Begin 在转发前持久化最少的请求元数据。
func (s *Store) Begin(entry, thread string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.db.Exec("INSERT INTO requests(started,entry,thread,phase,result,outcome,request_type) VALUES(?,?,?,'received','处理中','processing',CASE WHEN ?='GET /models' THEN 'auxiliary' ELSE 'unknown' END)", time.Now().UTC().UnixNano(), entry, thread, entry)
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	return int(id), err
}

// Provider 更新实际选择的供应商，不接受客户端自报供应商。
func (s *Store) Provider(id int, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("UPDATE requests SET provider=? WHERE id=?", provider, id)
	return err
}

// Finish 兼容旧调用；仅HTTP成功的历史不能升级为生成成功。
func (s *Store) Finish(id, status int, result string, elapsedMS int) error {
	outcome := "unconfirmed"
	if status < 200 || status >= 300 || !strings.HasPrefix(result, "HTTP_") {
		outcome = "error"
	}
	return s.FinishActivity(api.RequestActivity{ID: id, Status: status, Result: result, ElapsedMS: elapsedMS, Outcome: outcome, RequestType: "unknown", Metrics: api.RequestMetrics{KnownFields: []string{}}})
}

// FinishActivity 一次事务写入结果及用量快照，不保存上游原始事件。
func (s *Store) FinishActivity(item api.RequestActivity) error {
	if item.Metrics.KnownFields == nil {
		item.Metrics.KnownFields = []string{}
	}
	metrics, err := json.Marshal(item.Metrics)
	if err != nil {
		return err
	}
	observation, err := json.Marshal(item.Observation)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec("UPDATE requests SET phase='finished',result=?,status=?,elapsed=?,finished=?,outcome=?,model=?,request_type=?,error_stage=?,metrics=?,observation=? WHERE id=?", item.Result, item.Status, item.ElapsedMS, time.Now().UTC().UnixNano(), item.Outcome, item.Model, item.RequestType, item.ErrorStage, string(metrics), string(observation), item.ID)
	return err
}

// Requests 使用数据库索引和快照上界进行键集分页。
func (s *Store) Requests(limit int, raw, filter string) (api.RequestPage, error) {
	return s.requests(limit, raw, filter, 0, "")
}

// RequestNumberedPage 在首次ID上界内按页码查询；状态变化和清理仍实时可见。
func (s *Store) RequestNumberedPage(limit, page int, snapshot, filter string) (api.RequestPage, error) {
	if page < 1 || page > 1000000 {
		return api.RequestPage{}, ErrInvalidCursor
	}
	return s.requests(limit, "", filter, page, snapshot)
}

func (s *Store) requests(limit int, raw, filter string, pageNumber int, snapshot string) (api.RequestPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := api.RequestPage{Items: []api.RequestActivity{}}
	where := "1=1"
	switch filter {
	case "all":
	case "processing":
		where = "phase='received'"
	case "normal":
		where = "phase='finished' AND status BETWEEN 200 AND 299 AND (result LIKE 'HTTP_%' OR outcome='success')"
	case "error", "success", "unconfirmed":
		where = "phase='finished' AND outcome='" + filter + "'"
	default:
		return out, ErrInvalidCursor
	}
	if limit != 20 && limit != 50 && limit != 100 {
		return out, ErrInvalidCursor
	}
	c := cursor{Scope: "requests:" + filter}
	if pageNumber > 0 {
		c.Scope = fmt.Sprintf("request-page:%s:%d", filter, limit)
		if snapshot != "" {
			var err error
			c, err = decode(snapshot, c.Scope)
			if err != nil || c.ID != "snapshot" || c.Time != 1 {
				return out, ErrInvalidCursor
			}
		} else {
			if err := s.db.QueryRow("SELECT COALESCE(MAX(id),0) FROM requests").Scan(&c.Max); err != nil {
				return out, err
			}
			c.ID = "snapshot"
			c.Time = 1
		}
		out.Snapshot = encode(c)
	} else if raw != "" {
		var err error
		c, err = decode(raw, c.Scope)
		if err != nil {
			return out, err
		}
		if _, err = strconv.ParseInt(c.ID, 10, 64); err != nil {
			return out, ErrInvalidCursor
		}
	} else if err := s.db.QueryRow("SELECT COALESCE(MAX(id),0) FROM requests").Scan(&c.Max); err != nil {
		return out, err
	}
	base := where + " AND id<=?"
	if err := s.db.QueryRow("SELECT COUNT(*) FROM requests WHERE "+base, c.Max).Scan(&out.Total); err != nil {
		return out, err
	}
	out.TotalPages = (out.Total + limit - 1) / limit
	if pageNumber > 0 {
		out.Page = min(pageNumber, max(1, out.TotalPages))
	}
	args := []any{c.Max}
	if raw != "" {
		base += " AND (started<? OR (started=? AND id<?))"
		args = append(args, c.Time, c.Time, c.ID)
	}
	suffix := " ORDER BY started DESC,id DESC LIMIT ?"
	args = append(args, limit+1)
	if pageNumber > 0 {
		suffix += " OFFSET ?"
		args = append(args, (out.Page-1)*limit)
	}
	rows, err := s.db.Query("SELECT id,started,entry,thread,provider,phase,result,status,elapsed,outcome,model,request_type,error_stage,metrics,observation FROM requests WHERE "+base+suffix, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var lastTime int64
	for rows.Next() {
		var item api.RequestActivity
		var stamp int64
		var metrics, observation string
		if err = rows.Scan(&item.ID, &stamp, &item.Entry, &item.ThreadID, &item.ProviderID, &item.Phase, &item.Result, &item.Status, &item.ElapsedMS, &item.Outcome, &item.Model, &item.RequestType, &item.ErrorStage, &metrics, &observation); err != nil {
			return out, err
		}
		if err = json.Unmarshal([]byte(metrics), &item.Metrics); err != nil {
			return out, err
		}
		if err = json.Unmarshal([]byte(observation), &item.Observation); err != nil {
			return out, err
		}
		if item.Observation.MetricsStatus == "" {
			item.Observation.MetricsStatus = "legacy"
			if item.RequestType == "auxiliary" {
				item.Observation.MetricsStatus = "not_applicable"
			}
			if item.Phase == "received" {
				item.Observation.MetricsStatus = "pending"
			}
		}
		if item.Observation.TerminalResult == "" {
			item.Observation.TerminalResult = "unknown"
		}
		if item.Metrics.KnownFields == nil {
			item.Metrics.KnownFields = []string{}
		}
		if len(out.Items) == limit {
			if pageNumber > 0 {
				break
			}
			last := out.Items[len(out.Items)-1]
			out.NextCursor = encode(cursor{c.Scope, lastTime, strconv.Itoa(last.ID), c.Max})
			break
		}
		item.Time = time.Unix(0, stamp).UTC().Format(time.RFC3339Nano)
		lastTime = stamp
		out.Items = append(out.Items, item)
	}
	return out, rows.Err()
}

// Policy 返回请求历史保留天数；0 表示不限期。
func (s *Store) Policy() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var days int
	err := s.db.QueryRow("SELECT days FROM policy WHERE id=1").Scan(&days)
	return days, err
}

func prune(tx *sql.Tx, days int, now time.Time) (int, error) {
	if days == 0 {
		return 0, nil
	}
	r, err := tx.Exec("DELETE FROM requests WHERE phase='finished' AND started<? AND finished<=?", now.Add(-time.Duration(days)*24*time.Hour).UnixNano(), now.UnixNano())
	if err != nil {
		return 0, err
	}
	n, err := r.RowsAffected()
	return int(n), err
}

// SetPolicy 原子保存策略并按滚动 UTC 边界清理；失败全部回滚。
func (s *Store) SetPolicy(days int, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if days < 0 || days > 3650 {
		return 0, errors.New("保留天数须为0至3650")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE policy SET days=? WHERE id=1", days); err != nil {
		return 0, err
	}
	n, err := prune(tx, days, now.UTC())
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// Prune 执行当前策略，不清理活动请求、会话或绑定。
func (s *Store) Prune(now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var days int
	if err = tx.QueryRow("SELECT days FROM policy WHERE id=1").Scan(&days); err != nil {
		return 0, err
	}
	n, err := prune(tx, days, now.UTC())
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// Clear 仅清除操作开始前已结束的请求；删除后空间由后续写入复用。
func (s *Store) Clear(now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.db.Exec("DELETE FROM requests WHERE phase='finished' AND finished<=?", now.UTC().UnixNano())
	if err != nil {
		return 0, err
	}
	n, err := r.RowsAffected()
	return int(n), err
}

// SyncBindings 从权威 JSON 幂等重建绑定投影，保留其他待登记记录。
func (s *Store) SyncBindings(bindings []api.Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM sessions WHERE kind='bound'"); err != nil {
		return err
	}
	for _, b := range bindings {
		stamp, err := time.Parse(time.RFC3339Nano, b.CreatedAt)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO sessions(id,kind,provider,stamp,reason) VALUES(?,'bound',?,?,'') ON CONFLICT(id) DO UPDATE SET kind='bound',provider=excluded.provider,stamp=excluded.stamp,reason=''", b.ID, b.ProviderID, stamp.UnixNano()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Bind 同步已可靠写入 JSON 的单条绑定，移除同 ID 待登记投影。
func (s *Store) Bind(b api.Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp, err := time.Parse(time.RFC3339Nano, b.CreatedAt)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO sessions(id,kind,provider,stamp,reason) VALUES(?,'bound',?,?,'') ON CONFLICT(id) DO UPDATE SET kind='bound',provider=excluded.provider,stamp=excluded.stamp,reason=''", b.ID, b.ProviderID, stamp.UnixNano())
	return err
}

// PutPending 持久化未知来源元数据，不记录正文也不自动选择供应商。
func (s *Store) PutPending(p api.PendingSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp, err := time.Parse(time.RFC3339Nano, p.FirstSeen)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO sessions(id,kind,provider,stamp,reason) VALUES(?,'pending','',?,?) ON CONFLICT(id) DO NOTHING", p.ID, stamp.UnixNano(), p.Reason)
	return err
}

// Sessions 分别返回已绑定或待登记会话的数据库分页。
func (s *Store) Sessions(kind string, limit int, raw string) (api.SessionPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := api.SessionPage{Kind: kind, Bindings: []api.Binding{}, Pending: []api.PendingSession{}}
	if (kind != "bound" && kind != "pending") || (limit != 20 && limit != 50 && limit != 100) {
		return out, ErrInvalidCursor
	}
	var c cursor
	var err error
	if raw != "" {
		c, err = decode(raw, "sessions:"+kind)
		if err != nil {
			return out, err
		}
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE kind=?", kind).Scan(&out.Total); err != nil {
		return out, err
	}
	where := "kind=?"
	args := []any{kind}
	if raw != "" {
		where += " AND (stamp<? OR (stamp=? AND id<?))"
		args = append(args, c.Time, c.Time, c.ID)
	}
	args = append(args, limit+1)
	rows, err := s.db.Query("SELECT id,provider,stamp,reason FROM sessions WHERE "+where+" ORDER BY stamp DESC,id DESC LIMIT ?", args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var last cursor
	count := 0
	for rows.Next() {
		var id, provider, reason string
		var stamp int64
		if err = rows.Scan(&id, &provider, &stamp, &reason); err != nil {
			return out, err
		}
		if count == limit {
			out.NextCursor = encode(last)
			break
		}
		text := time.Unix(0, stamp).UTC().Format(time.RFC3339Nano)
		if kind == "bound" {
			out.Bindings = append(out.Bindings, api.Binding{ID: id, ProviderID: provider, CreatedAt: text})
		} else {
			out.Pending = append(out.Pending, api.PendingSession{ID: id, FirstSeen: text, Reason: reason})
		}
		last = cursor{Scope: "sessions:" + kind, Time: stamp, ID: id}
		count++
	}
	return out, rows.Err()
}
