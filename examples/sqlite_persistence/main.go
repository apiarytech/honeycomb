/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v2.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// This example shows the PLC persistence lifecycle with SQLite: restore tags at
// power-up, write changes behind the scan cycle, and save a final snapshot at
// shutdown. Run it several times; the counter continues where it stopped.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"time"

	tags "github.com/apiarytech/honeycomb"
	"github.com/apiarytech/honeycomb/shared"
	"github.com/apiarytech/honeycomb/store/sqlstore"
	plc "github.com/apiarytech/royaljelly"
	_ "modernc.org/sqlite" // Pure-Go SQLite driver: no CGO needed.
)

func main() {
	ctx := context.Background()

	// --- Power-up -----------------------------------------------------------
	// 1. Configure tags exactly as the PLC program declares them.
	db := tags.NewTagDatabase()
	shared.PopulateDB(db)
	db.AddTag(&tags.Tag{Name: "ScanCount", TypeInfo: &tags.TypeInfo{DataType: tags.TypeLINT}, Value: plc.LINT(0), Retain: true})

	// 2. Open the store. AutoSetup creates or upgrades the schema.
	//    WAL and busy_timeout make SQLite robust against power loss and contention.
	store, err := sqlstore.Open(ctx, "sqlite",
		"file:plc.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)",
		sqlstore.Options{InstanceID: "line1-plc", AutoSetup: true})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	// 3. Attach the store and restore the values saved by the previous run.
	persister, err := db.AttachStore(store, tags.PersistOptions{
		Scope:         tags.PersistRetainOnly,
		FlushInterval: 500 * time.Millisecond,
		OnError:       func(err error) { log.Printf("persist: %v", err) },
	})
	if err != nil {
		log.Fatalf("attach store: %v", err)
	}
	if err := persister.Restore(ctx); err != nil {
		log.Printf("restore: %v", err)
	}
	start, _ := db.GetTagValue("ScanCount")
	log.Printf("Power-up: ScanCount restored to %v", start)

	// 4. Start write-behind persistence, then the scan cycle.
	persister.Start()

	// --- Runtime ------------------------------------------------------------
	stop, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()
	scan := time.NewTicker(10 * time.Millisecond)
	defer scan.Stop()
	deadline := time.After(3 * time.Second)

	for running := true; running; {
		select {
		case <-scan.C:
			count, _ := db.GetTagValue("ScanCount")
			db.SetTagValue("ScanCount", count.(plc.LINT)+1) // Never blocks on the database.
		case <-deadline:
			running = false
		case <-stop.Done():
			running = false
		}
	}

	// --- Shutdown -----------------------------------------------------------
	// Close stops the background writer, saves every retained tag and closes the store.
	shutdownCtx, cancelShutdown := context.WithTimeout(ctx, 5*time.Second)
	defer cancelShutdown()
	end, _ := db.GetTagValue("ScanCount")
	if err := persister.Close(shutdownCtx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
	log.Printf("Shutdown: ScanCount %v saved to plc.db", end)
}
