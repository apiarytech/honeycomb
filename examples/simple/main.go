package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	tags "github.com/apiarytech/honeycomb"
	"github.com/apiarytech/honeycomb/examples/shared"
	plc "github.com/apiarytech/royaljelly"
)

func main() {
	fmt.Println("--- tags TagDatabase Usage Example ---")

	// Register custom types before using them.
	tags.RegisterUDT(&shared.MotorData{})
	tags.RegisterENUM("MotorState", []string{"Stopped", "Running", "Faulted"})
	fmt.Println("Registered UDT 'MotorData' and ENUM 'MotorState'.")

	// --- 2. Initialize Database and Add Tags ---
	db := tags.NewTagDatabase()
	fmt.Println("\n--- Creating and Adding Tags ---")

	// Add a simple DINT tag
	dintTag := &tags.Tag{
		Name:  "MyDINT",
		Value: plc.DINT(100),
		TypeInfo: &tags.TypeInfo{
			DataType: tags.TypeDINT,
		},
		Retain: true, // Mark for persistence
	}
	if err := db.AddTag(dintTag); err != nil {
		fmt.Printf("Error adding MyDINT: %v\n", err)
	}
	fmt.Println("Added tag 'MyDINT' with value 100.")

	// Add an array of UDTs
	motorArrayValue := []*shared.MotorData{
		{Speed: 1500.0, Current: 30.5, Running: true},
		{Speed: 0.0, Current: 0.1, Running: false},
	}
	motorArrayTag := &tags.Tag{
		Name: "MotorLine",
		TypeInfo: &tags.TypeInfo{
			DataType:    tags.TypeARRAY,
			ElementType: "MotorData", // The registered UDT name
		},
		Value:  motorArrayValue,
		Retain: true, // Mark for persistence
	}
	if err := db.AddTag(motorArrayTag); err != nil {
		fmt.Printf("Error adding MotorLine: %v\n", err)
	}
	fmt.Println("Added tag 'MotorLine' (an array of MotorData).")

	// --- 3. Accessing and Modifying Tag Values ---
	fmt.Println("\n--- Accessing and Modifying Tag Values ---")

	// Get a tag's value
	val, err := db.GetTagValue("MyDINT")
	if err != nil {
		fmt.Printf("Error getting MyDINT: %v\n", err)
	} else {
		fmt.Printf("Initial value of 'MyDINT': %v\n", val)
	}

	// Set a tag's value
	err = db.SetTagValue("MyDINT", plc.DINT(200))
	if err != nil {
		fmt.Printf("Error setting MyDINT: %v\n", err)
	}
	val, _ = db.GetTagValue("MyDINT")
	fmt.Printf("New value of 'MyDINT': %v\n", val)

	// Access a field on a UDT within an array
	motorSpeed, err := db.GetTagValue("MotorLine[0].Speed")
	if err != nil {
		fmt.Printf("Error getting MotorLine[0].Speed: %v\n", err)
	} else {
		fmt.Printf("Value of 'MotorLine[0].Speed': %v\n", motorSpeed)
	}

	// Set a field on a UDT within an array
	err = db.SetTagValue("MotorLine[1].Running", plc.BOOL(true))
	if err != nil {
		fmt.Printf("Error setting MotorLine[1].Running: %v\n", err)
	}
	runningStatus, _ := db.GetTagValue("MotorLine[1].Running")
	fmt.Printf("New value of 'MotorLine[1].Running': %v\n", runningStatus)

	// --- 4. Persistence ---
	fmt.Println("\n--- Persistence ---")
	// Get the directory of the currently running file to make the path relative.
	_, b, _, _ := runtime.Caller(0)
	basepath := filepath.Dir(b)
	filePath := filepath.Join(basepath, "tags.txt")

	fmt.Printf("Using persistence file: %s\n", filePath)

	// Write all retain-qualified tags to a file
	if err := db.WriteTagsToFile(filePath); err != nil {
		fmt.Printf("Error writing tags to file: %v\n", err)
	} else {
		fmt.Printf("Wrote retain-qualified tags to '%s'.\n", filePath)
	}

	// Create a new database to simulate an application restart
	dbRead := tags.NewTagDatabase()
	// Pre-add tags so the database knows their types before reading values
	dbRead.AddTag(&tags.Tag{Name: "MyDINT", TypeInfo: &tags.TypeInfo{DataType: tags.TypeDINT}, Retain: true})
	dbRead.AddTag(&tags.Tag{Name: "MotorLine", TypeInfo: &tags.TypeInfo{DataType: tags.TypeARRAY, ElementType: "MotorData"}, Retain: true})

	// Read tags from the file
	err = dbRead.ReadTagsFromFile(filePath)
	if err != nil {
		fmt.Printf("Error reading tags from file: %v\n", err)
	} else {
		fmt.Printf("Read tags from '%s' into a new database instance.\n", filePath)
	}

	// Verify the read value
	readVal, _ := dbRead.GetTagValue("MyDINT")
	fmt.Printf("Value of 'MyDINT' in new database: %v\n", readVal)
	readMotorStatus, _ := dbRead.GetTagValue("MotorLine[1].Running")
	fmt.Printf("Value of 'MotorLine[1].Running' in new database: %v\n", readMotorStatus)

	// Clean up the created file
	os.Remove(filePath)
	fmt.Printf("Cleaned up '%s'.\n", filePath)

	// --- 5. Cross-Database Aliasing ---
	fmt.Println("\n--- Cross-Database Aliasing ---")

	// In Service A, which has a TagDatabase instance `db1`
	db1 := tags.NewTagDatabase()
	db1.AddTag(&tags.Tag{Name: "SourceTag", Value: plc.DINT(100), TypeInfo: &tags.TypeInfo{DataType: tags.TypeDINT}, Retain: true})
	fmt.Println("Created db1 with 'SourceTag' value 100.")

	// In Service B, which has a TagDatabase instance `db2`
	db2 := tags.NewTagDatabase()

	// First, register db1 with db2
	db2.RegisterDatabase("DB1_ID", db1)
	fmt.Println("Registered db1 with db2.")

	// Now, create an alias that points to the tag in db1
	db2.AddTag(&tags.Tag{Name: "AliasToDB1", IsRemoteAlias: true, RemoteDBID: "DB1_ID", RemoteTagName: "SourceTag", Retain: true})
	fmt.Println("Created 'AliasToDB1' in db2 pointing to 'SourceTag' in db1.")

	// Reading "AliasToDB1" in db2 will now transparently access "SourceTag" in db1.
	aliasVal, err := db2.GetTagValue("AliasToDB1")
	if err != nil {
		fmt.Printf("Error getting alias value: %v\n", err)
	} else {
		fmt.Printf("Read value via alias 'AliasToDB1': %v (should be 100)\n", aliasVal)
	}

	// Writing to the alias in db2 will modify the source tag in db1.
	fmt.Println("Setting value of 'AliasToDB1' to 500.")
	db2.SetTagValue("AliasToDB1", plc.DINT(500))

	// Verify the change in the original database
	sourceVal, _ := db1.GetTagValue("SourceTag")
	fmt.Printf("New value of 'SourceTag' in db1: %v (should be 500)\n", sourceVal)

	fmt.Println("\n--- Example Finished ---")
}
