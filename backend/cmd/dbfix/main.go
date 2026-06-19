package main

import (
	"database/sql"
	"fmt"
	"log"

	_ "github.com/lib/pq"
)

func main() {
	dsn := "host=localhost port=5432 user=user password=root dbname=testdb sslmode=disable"
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		log.Fatalf("ping: %v", err)
	}

	// Before
	var chk string
	switch err := db.QueryRow(`SELECT checksum FROM schema_migrations WHERE filename='154_routing_strategies.sql'`).Scan(&chk); err {
	case nil:
		fmt.Printf("BEFORE: schema_migrations record exists, checksum=%s\n", chk)
	case sql.ErrNoRows:
		fmt.Println("BEFORE: no schema_migrations record for 154")
	default:
		log.Fatalf("select record: %v", err)
	}

	var tableExists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='routing_strategies')`).Scan(&tableExists); err != nil {
		log.Fatalf("check table: %v", err)
	}
	fmt.Printf("BEFORE: routing_strategies table exists=%v\n", tableExists)

	// Cleanup
	if _, err := db.Exec(`DROP TABLE IF EXISTS routing_strategies`); err != nil {
		log.Fatalf("drop table: %v", err)
	}
	res, err := db.Exec(`DELETE FROM schema_migrations WHERE filename='154_routing_strategies.sql'`)
	if err != nil {
		log.Fatalf("delete record: %v", err)
	}
	n, _ := res.RowsAffected()
	fmt.Printf("CLEANUP: dropped routing_strategies, deleted %d schema_migrations row(s)\n", n)

	// After
	var after int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE filename='154_routing_strategies.sql'`).Scan(&after); err != nil {
		log.Fatalf("verify: %v", err)
	}
	fmt.Printf("AFTER: schema_migrations rows for 154 = %d (expect 0)\n", after)
}
