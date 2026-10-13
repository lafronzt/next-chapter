package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// A migration moves the database from version-1 to version. Each one runs in its
// own BEGIN IMMEDIATE transaction together with the schema_version update, so a
// failure leaves the previous version fully intact.
type migration struct {
	version int
	name    string
	apply   func(*sql.Tx) error
}

func latestVersion(list []migration) int {
	if len(list) == 0 {
		return 0
	}
	return list[len(list)-1].version
}

// schemaVersion reports the stored version. Databases written by the Go app
// before the migration runner existed carry go_data_version instead; they are
// at version 2 and legacy reports that the old key still needs removing.
func schemaVersion(q queryer) (version int, legacy bool, err error) {
	var tables int
	if err = q.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='meta'").Scan(&tables); err != nil || tables == 0 {
		return 0, false, err
	}
	if version, err = readMeta(q, "schema_version", 1); err != nil {
		return 0, false, err
	}
	goData, err := readMeta(q, "go_data_version", 0)
	if err != nil {
		return 0, false, err
	}
	if goData >= 1 {
		legacy = true
		version = max(version, 2)
	}
	return version, legacy, nil
}

func recordVersion(tx *sql.Tx, version int) error {
	if err := setMeta(tx, "schema_version", version); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM meta WHERE key='go_data_version'")
	return err
}

// migrate brings the database at path up to the last version in list. db must
// begin transactions with BEGIN IMMEDIATE (see migrationDSN) so the version is
// read under the write lock, and must allow a second connection for backups.
func migrate(db *sql.DB, list []migration, path string) error {
	for i, m := range list {
		if m.version != i+1 {
			return fmt.Errorf("migration list is not contiguous at %q", m.name)
		}
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// A database that starts at version 0 is new and has nothing to back up.
	latest, backedUp, first := latestVersion(list), false, true
	for {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		version, legacy, err := schemaVersion(tx)
		if err != nil {
			tx.Rollback()
			return err
		}
		if version > latest {
			tx.Rollback()
			return fmt.Errorf("database schema version %d is newer than this app supports (%d); run a newer release or restore the backup made before upgrading", version, latest)
		}
		if version == latest {
			if !legacy {
				return tx.Rollback()
			}
			if err = recordVersion(tx, version); err != nil {
				tx.Rollback()
				return err
			}
			return tx.Commit()
		}
		if first {
			backedUp, first = version == 0, false
		}
		next := list[version]
		if !backedUp {
			// The open IMMEDIATE transaction blocks other writers, so this snapshot
			// taken on a second connection is exactly the pre-migration state.
			backup, err := backupBeforeMigration(db, path, next.version)
			if err != nil {
				tx.Rollback()
				return fmt.Errorf("back up database before migration %d: %w", next.version, err)
			}
			log.Printf("Backed up database to %s before schema migration %d", backup, next.version)
			backedUp = true
		}
		if err = next.apply(tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("schema migration %d (%s): %w", next.version, next.name, err)
		}
		if err = recordVersion(tx, next.version); err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		log.Printf("Applied schema migration %d: %s", next.version, next.name)
	}
}

func backupBeforeMigration(db *sql.DB, path string, version int) (string, error) {
	base := fmt.Sprintf("%s.pre-v%d", path, version)
	target := base + ".bak"
	for n := 1; ; n++ {
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return "", err
		}
		target = fmt.Sprintf("%s.%d.bak", base, n)
	}
	if _, err := db.Exec("VACUUM INTO ?", target); err != nil {
		return "", err
	}
	if err := os.Chmod(target, 0600); err != nil {
		return "", err
	}
	return target, nil
}

// migrationDSN makes every BEGIN an IMMEDIATE one, taking the write lock first.
// It deliberately leaves the journal mode alone: switching to WAL fails at once
// with SQLITE_BUSY, without waiting, while another process holds a lock.
func migrationDSN(path string) string {
	return path + "?_txlock=immediate&_pragma=busy_timeout(15000)"
}

func migrateFile(path string, list []migration) error {
	db, err := sql.Open("sqlite", migrationDSN(path))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	if err = migrate(db, list, path); err != nil {
		return err
	}
	return enableWAL(db)
}

// enableWAL retries because the journal mode change does not use busy_timeout.
func enableWAL(db *sql.DB) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		var mode string
		err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode)
		if err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
