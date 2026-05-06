package cache

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type sqliteIndex struct {
	db *sql.DB
}

// NewSQLiteIndex opens or creates a SQLite database at dbPath.
func NewSQLiteIndex(dbPath string) (Index, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS entries (
			build_id    TEXT NOT NULL,
			kind        INTEGER NOT NULL,
			size        INTEGER NOT NULL,
			last_access INTEGER NOT NULL,
			PRIMARY KEY (build_id, kind)
		);
		CREATE INDEX IF NOT EXISTS idx_last_access ON entries(last_access);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &sqliteIndex{db: db}, nil
}

func (s *sqliteIndex) Touch(buildID string, kind Kind, size int64) error {
	now := time.Now().UnixNano()
	_, err := s.db.Exec(`
		INSERT INTO entries (build_id, kind, size, last_access)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(build_id, kind) DO UPDATE SET
			size = excluded.size,
			last_access = excluded.last_access
	`, buildID, int(kind), size, now)
	return err
}

func (s *sqliteIndex) TotalBytes() (int64, error) {
	var total sql.NullInt64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM entries`).Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

func (s *sqliteIndex) EvictTo(maxBytes int64) ([]Entry, error) {
	total, err := s.TotalBytes()
	if err != nil {
		return nil, err
	}
	if total <= maxBytes {
		return nil, nil
	}
	excess := total - maxBytes

	rows, err := s.db.Query(`SELECT build_id, kind, size, last_access FROM entries ORDER BY last_access ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var evicted []Entry
	var freed int64
	for rows.Next() && freed < excess {
		var e Entry
		var k int
		var ns int64
		if err := rows.Scan(&e.BuildID, &k, &e.Size, &ns); err != nil {
			return evicted, err
		}
		e.Kind = Kind(k)
		e.LastAccess = time.Unix(0, ns)
		evicted = append(evicted, e)
		freed += e.Size
	}
	if err := rows.Err(); err != nil {
		return evicted, err
	}
	rows.Close()

	for _, e := range evicted {
		if _, err := s.db.Exec(`DELETE FROM entries WHERE build_id=? AND kind=?`, e.BuildID, int(e.Kind)); err != nil {
			return evicted, err
		}
	}
	return evicted, nil
}

func (s *sqliteIndex) Iter(yield func(Entry) bool) error {
	rows, err := s.db.Query(`SELECT build_id, kind, size, last_access FROM entries`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var k int
		var ns int64
		if err := rows.Scan(&e.BuildID, &k, &e.Size, &ns); err != nil {
			return err
		}
		e.Kind = Kind(k)
		e.LastAccess = time.Unix(0, ns)
		if !yield(e) {
			return nil
		}
	}
	return rows.Err()
}

func (s *sqliteIndex) Forget(buildID string, kind Kind) error {
	_, err := s.db.Exec(`DELETE FROM entries WHERE build_id=? AND kind=?`, buildID, int(kind))
	return err
}

func (s *sqliteIndex) Close() error { return s.db.Close() }
