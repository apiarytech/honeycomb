/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v2.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// Package shared contains common code used by multiple examples.
package shared

import (
	"context"
	"log"
	"sync"

	tags "github.com/apiarytech/honeycomb"
	plc "github.com/apiarytech/royaljelly"
)

// MotorState represents the possible states of a motor.
var MotorState = []string{"Stopped", "Running", "Faulted"}

// MotorData is a custom User-Defined Type (UDT) representing motor telemetry.
type MotorData struct {
	Speed   plc.REAL
	Current plc.REAL
	Running plc.BOOL
}

// TypeName implements the tags.UDT interface for MotorData.
func (m *MotorData) TypeName() tags.DataType {
	return "MotorData"
}

// PopulateDB registers custom types and adds a set of sample tags to a TagDatabase.
// This ensures a consistent tag set for the networking examples.
func PopulateDB(db *tags.TagDatabase) {
	// 1. Register custom types with the honeycomb system.
	tags.RegisterUDT(&MotorData{})
	tags.RegisterENUM("MotorState", MotorState)

	// 2. Add a simple DINT tag.
	db.AddTag(&tags.Tag{
		Name:     "MyDINT",
		TypeInfo: &tags.TypeInfo{DataType: tags.TypeDINT},
		Value:    plc.DINT(42),
		Retain:   true,
	})

	// 3. Add an array of MotorData UDTs.
	db.AddTag(&tags.Tag{
		Name: "MotorLine",
		TypeInfo: &tags.TypeInfo{
			DataType:    tags.TypeARRAY,
			ElementType: "MotorData",
		},
		Value: []*MotorData{
			{Speed: 1800.5, Current: 55.2, Running: true},
			{Speed: 0.0, Current: 1.2, Running: false},
		},
		Retain: true,
	})

	log.Println("[Server] Database populated with sample tags (MyDINT, MotorLine).")
}

// StartServer is a helper function used by the network_client example to launch
// a server instance in the background for testing purposes.
func StartServer(ctx context.Context, serverReady *sync.WaitGroup) {
	db := tags.NewTagDatabase()
	// Ensure the server started by the client example has the same tags
	// as the standalone server example.
	PopulateDB(db)

	log.Println("[Server] TagDatabase initialized with sample tags.")

	port := "8080"
	certFile := "../shared/server.crt"
	keyFile := "../shared/server.key"
	validTokens := []string{"super-secret-token-123"}

	tags.StartServer(db, validTokens, port, certFile, keyFile, serverReady, ctx)
}
