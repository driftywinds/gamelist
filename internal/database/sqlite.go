package database

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3" // SQLite driver
)

// DB is a wrapper for the database connection.
type DB struct {
	*sql.DB
}

// NewSQLiteDB initializes a new SQLite database connection.
// If the database file does not exist, it will be created.
func NewSQLiteDB(dataSourceName string) (*DB, error) {
	// Check if the directory for the database exists, create if not
	dbDir := os.Dir(dataSourceName)
	if dbDir != "" {
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create database directory %s: %w", dbDir, err)
		}
	}

	db, err := sql.Open("sqlite3", dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Optional: Ping the database to verify the connection
	if err := db.Ping(); err != nil {
		db.Close() // Close the connection if ping fails
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Optional: Run migrations here if they are not handled by a separate tool
	// if err := runMigrations(db); err != nil {
	// 	db.Close()
	// 	return nil, fmt.Errorf("failed to run migrations: %w", err)
	// }

	return &DB{db}, nil
}

// Close closes the database connection.
func (db *DB) Close() error {
	return db.DB.Close()
}

// Example of a migration function (you'd typically use a dedicated migration tool)
// func runMigrations(db *sql.DB) error {
// 	// SQL statements for creating tables
// 	createGamesTable := `
// 	CREATE TABLE IF NOT EXISTS games (
// 		id INTEGER PRIMARY KEY AUTOINCREMENT,
// 		title TEXT NOT NULL,
// 		description TEXT,
// 		release_date TEXT,
// 		developers TEXT,
// 		publishers TEXT,
// 		platforms TEXT,
// 		owned_stores TEXT
// 	);`
//
// 	_, err := db.Exec(createGamesTable)
// 	if err != nil {
// 		return fmt.Errorf("failed to create games table: %w", err)
// 	}
//
// 	// Add more CREATE TABLE statements for other tables (users, store_credentials, etc.)
//
// 	return nil
// }
