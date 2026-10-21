//go:build memory_sqlite

package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// IndexDriver names the index of the notes that this build carries.
const IndexDriver = "sqlite-fts5"

// ftsIndex is the SQLite FTS5 index of the notes. Each scope has one
// database file under <home>/memory/index. The notes stay canonical; a
// database can be deleted at any time and the next search builds it again.
// Several Stores of one process, and several processes, can open the same
// file: every write runs in an immediate transaction with a busy timeout.
type ftsIndex struct {
	mu  sync.Mutex
	dbs map[string]*sql.DB // by the directory of the index
}

func newNoteIndex(string) noteIndex { return &ftsIndex{dbs: map[string]*sql.DB{}} }

// schemaVersion is the version of the database. A file with another version
// is built again from the notes.
const schemaVersion = 2

// applicationID marks a database of this index ("FWME").
const applicationID = 0x46574D45

// Search syncs the notes of notesDir into the index and queries it. When the
// database turns out to be corrupt or of another shape, the index builds it
// again from the notes once.
func (x *ftsIndex) Search(ctx context.Context, scope fw.MemoryScope, notesDir, indexDir string, terms []string, limit int) ([]fw.MemoryHit, error) {
	match := ftsMatch(terms)
	if match == "" {
		return nil, nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	hits, err := x.search(ctx, scope, notesDir, indexDir, match, limit)
	if err != nil && rebuildable(err) {
		x.drop(indexDir)
		hits, err = x.search(ctx, scope, notesDir, indexDir, match, limit)
	}
	return hits, err
}

func (x *ftsIndex) search(ctx context.Context, scope fw.MemoryScope, notesDir, indexDir, match string, limit int) ([]fw.MemoryHit, error) {
	db, err := x.open(indexDir)
	if err != nil {
		return nil, err
	}
	if err := syncNotes(ctx, db, notesDir); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT n.path, n.title, n.mtime, n.size,
		       snippet(notes_fts, 2, '', '', '...', 16),
		       bm25(notes_fts, 6.0, 3.0, 1.0)
		FROM notes_fts JOIN notes n ON n.id = notes_fts.rowid
		WHERE notes_fts MATCH ?
		ORDER BY bm25(notes_fts, 6.0, 3.0, 1.0)
		LIMIT ?`, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []fw.MemoryHit
	for rows.Next() {
		var name, title, snippet string
		var mtime, size int64
		var rank float64
		if err := rows.Scan(&name, &title, &mtime, &size, &snippet, &rank); err != nil {
			return nil, err
		}
		hits = append(hits, fw.MemoryHit{
			Record: fw.MemoryRecord{
				Ref:         NoteRef(scope, name),
				Kind:        fw.MemoryKindNote,
				Scope:       scope,
				Title:       title,
				Description: title,
				Source:      "memory",
				Trust:       "untrusted",
				Size:        size,
				Updated:     time.Unix(0, mtime),
			},
			// bm25 is lower for a better match. A note always ranks above
			// a run or a file with the same words.
			Score:   10 - rank,
			Snippet: strings.Join(strings.Fields(snippet), " "),
			Why:     "fts5",
		})
	}
	return hits, rows.Err()
}

// Close closes every database of the index.
func (x *ftsIndex) Close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	var errs []error
	for dir, db := range x.dbs {
		if err := db.Close(); err != nil {
			errs = append(errs, fmt.Errorf("memory: index %s: %w", dir, err))
		}
		delete(x.dbs, dir)
	}
	return errors.Join(errs...)
}

// drop closes the database of one scope and removes its files.
func (x *ftsIndex) drop(dir string) {
	if db, ok := x.dbs[dir]; ok {
		_ = db.Close()
		delete(x.dbs, dir)
	}
	removeDB(filepath.Join(dir, "notes.db"))
}

func removeDB(file string) {
	for _, f := range []string{file, file + "-journal", file + "-wal", file + "-shm"} {
		_ = os.Remove(f)
	}
}

// open returns the database of one scope. It checks the marks of the file
// and builds a file of another kind again, so it never writes into a
// database that is not its own.
func (x *ftsIndex) open(dir string) (*sql.DB, error) {
	if db, ok := x.dbs[dir]; ok {
		return db, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("memory: index directory: %w", err)
	}
	file := filepath.Join(dir, "notes.db")
	db, err := openDB(file)
	if err != nil {
		return nil, err
	}
	own, err := ownDB(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if !own {
		// The file is of another version, not of this index, or not a
		// database. It is derived, so it goes, and the notes build it again.
		db.Close()
		removeDB(file)
		if db, err = openDB(file); err != nil {
			return nil, err
		}
	}
	// One immediate transaction sets the marks and the schema, so another
	// Store that opens the file at the same moment sees an empty file or a
	// whole index, never a half that looks foreign.
	if err := prepare(db); err != nil {
		db.Close()
		return nil, err
	}
	x.dbs[dir] = db
	return db, nil
}

// prepare sets the marks and the schema of the index in one transaction.
func prepare(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("memory: prepare index: %w", err)
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		fmt.Sprintf(`PRAGMA application_id = %d`, applicationID),
		fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion),
		`CREATE TABLE IF NOT EXISTS notes (
			id INTEGER PRIMARY KEY,
			path TEXT NOT NULL UNIQUE,
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			mtime INTEGER NOT NULL,
			size INTEGER NOT NULL,
			ino INTEGER NOT NULL
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS notes_fts USING fts5(
			path, title, body,
			content='notes', content_rowid='id',
			tokenize='unicode61 remove_diacritics 2'
		)`,
		`CREATE TRIGGER IF NOT EXISTS notes_ai AFTER INSERT ON notes BEGIN
			INSERT INTO notes_fts(rowid, path, title, body) VALUES (new.id, new.path, new.title, new.body);
		END`,
		`CREATE TRIGGER IF NOT EXISTS notes_ad AFTER DELETE ON notes BEGIN
			INSERT INTO notes_fts(notes_fts, rowid, path, title, body) VALUES ('delete', old.id, old.path, old.title, old.body);
		END`,
		`CREATE TRIGGER IF NOT EXISTS notes_au AFTER UPDATE ON notes BEGIN
			INSERT INTO notes_fts(notes_fts, rowid, path, title, body) VALUES ('delete', old.id, old.path, old.title, old.body);
			INSERT INTO notes_fts(rowid, path, title, body) VALUES (new.id, new.path, new.title, new.body);
		END`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("memory: prepare index: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: prepare index: %w", err)
	}
	return nil
}

// ownDB reports whether a database is of this index and version, or empty.
// A file that is not a database, or that is corrupt, is not its own. Any
// other error, such as a busy lock of another writer, is an error and never
// a reason to remove the file.
func ownDB(db *sql.DB) (bool, error) {
	var app, version, objects int
	err := db.QueryRow(`PRAGMA application_id`).Scan(&app)
	if err == nil {
		err = db.QueryRow(`PRAGMA user_version`).Scan(&version)
	}
	if err == nil {
		err = db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&objects)
	}
	if err != nil {
		if sqliteCode(err) == sqlite3.SQLITE_NOTADB || sqliteCode(err) == sqlite3.SQLITE_CORRUPT {
			return false, nil
		}
		return false, err
	}
	if app == applicationID && version == schemaVersion {
		return true, nil
	}
	return app == 0 && version == 0 && objects == 0, nil
}

// sqliteCode returns the primary result code of a SQLite error, or 0.
func sqliteCode(err error) int {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() & 0xff
	}
	return 0
}

// rebuildable reports whether an error means that the database is corrupt
// or of another shape, so a new file from the notes can answer.
func rebuildable(err error) bool {
	switch sqliteCode(err) {
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no such column") || strings.Contains(msg, "no such table") || strings.Contains(msg, "malformed")
}

// openDB opens one database file with one connection, a busy timeout,
// immediate transactions, and a rollback journal. A journal in WAL mode
// needs shared memory, which a sandbox volume does not always give. An idle
// connection closes after a minute, so the file of a scope that nobody
// searches holds no descriptor.
func openDB(file string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", file+"?_busy_timeout=5000&_txlock=immediate&_pragma=journal_mode(DELETE)")
	if err != nil {
		return nil, fmt.Errorf("memory: open index: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(time.Minute)
	return db, nil
}

// syncNotes reads the notes of dir and updates the rows that changed, by
// modification time, size, and inode. It removes the rows whose notes are
// gone.
func syncNotes(ctx context.Context, db *sql.DB, dir string) error {
	notes, err := walkNotes(ctx, dir, 0)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	known := map[string][3]int64{}
	rows, err := tx.QueryContext(ctx, `SELECT path, mtime, size, ino FROM notes`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		var mtime, size, ino int64
		if err := rows.Scan(&name, &mtime, &size, &ino); err != nil {
			rows.Close()
			return err
		}
		known[name] = [3]int64{mtime, size, ino}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var files fs.FS
	if len(notes) > 0 {
		f, err := fw.DirFiles(dir)
		if err != nil {
			return err
		}
		files = f
	}
	for _, n := range notes {
		mark := [3]int64{n.mod.UnixNano(), n.size, int64(n.ino)}
		if old, ok := known[n.name]; ok {
			delete(known, n.name)
			if old == mark {
				continue
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO notes(path, title, body, mtime, size, ino) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET
				title = excluded.title, body = excluded.body,
				mtime = excluded.mtime, size = excluded.size, ino = excluded.ino`,
			n.name, firstLine(files, n.name), readBounded(files, n.name, MaxNoteBytes), mark[0], mark[1], mark[2])
		if err != nil {
			return err
		}
	}
	for name := range known {
		if _, err := tx.ExecContext(ctx, `DELETE FROM notes WHERE path = ?`, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ftsMatch turns words into an FTS5 query. Each word is a quoted string,
// so a word cannot add an FTS5 operator.
func ftsMatch(terms []string) string {
	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		if term = strings.TrimSpace(term); term != "" {
			parts = append(parts, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
		}
	}
	return strings.Join(parts, " OR ")
}
