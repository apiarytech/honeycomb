/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under:
 * - GPL v2.0
 * - Commercial
 *
 * You may choose to use this software under the terms of either license.
 * See the LICENSE files in the project root for full license text.
 */

package honeycomb

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	plc "github.com/apiarytech/royaljelly" // Assuming this is an external dependency
)

// TestNewTagDatabase verifies that the constructor creates a valid, empty database.
func TestNewTagDatabase(t *testing.T) {
	db := NewTagDatabase()
	if db == nil {
		t.Fatal("NewTagDatabase() returned nil")
	}
	if count := len(db.GetAllTags()); count != 0 {
		t.Errorf("NewTagDatabase() should create an empty map, but size is %d", count)
	}
}

// TestAddAndGetTag tests adding a new tag and retrieving it.
func TestAddAndGetTag(t *testing.T) {
	db := NewTagDatabase() // Create a new TagDatabase instance
	tag := &Tag{
		Name:  "TestTag1",
		Alias: "TT1",
		TypeInfo: &TypeInfo{
			DataType: TypeDINT,
		},
		Description: "A test tag",
		Forced:      false,
	}

	// Test adding a new tag
	err := db.AddTag(tag)
	if err != nil {
		t.Fatalf("AddTag() returned an unexpected error: %v", err) // Pass by pointer
	}

	// Test retrieving the added tag
	retrievedTag, found := db.GetTag("TestTag1")
	if !found {
		t.Fatal("GetTag() did not find a tag that was just added")
	}
	if retrievedTag.Name != tag.Name {
		t.Errorf("GetTag() returned tag with wrong name. Got %s, want %s", retrievedTag.Name, tag.Name)
	}
	if retrievedTag.TypeInfo.DataType != tag.TypeInfo.DataType {
		t.Errorf("GetTag() returned tag with wrong DataType. Got %s, want %s", retrievedTag.TypeInfo.DataType, tag.TypeInfo.DataType)
	}

	// Test getting a non-existent tag
	_, found = db.GetTag("NonExistentTag")
	if found {
		t.Error("GetTag() found a non-existent tag")
	}
}

// TestAddDuplicateTag tests that adding a tag with a duplicate name returns an error.
func TestAddDuplicateTag(t *testing.T) {
	db := NewTagDatabase()
	tag1 := &Tag{Name: "DuplicateTag", TypeInfo: &TypeInfo{DataType: TypeBOOL}}
	tag2 := &Tag{Name: "DuplicateTag", TypeInfo: &TypeInfo{DataType: TypeINT}}

	err1 := db.AddTag(tag1)
	if err1 != nil {
		t.Fatalf("AddTag() failed on first add: %v", err1)
	}

	err2 := db.AddTag(tag2)
	if err2 == nil {
		t.Fatal("AddTag() did not return an error when adding a duplicate tag")
	}

	expectedError := fmt.Sprintf("tag '%s' already exists in the database", tag1.Name)
	if err2.Error() != expectedError {
		t.Errorf("AddTag() returned wrong error message. Got '%s', want '%s'", err2.Error(), expectedError)
	}
}

// TestGetAllTags verifies that all tags can be retrieved correctly.
func TestGetAllTags(t *testing.T) {
	db := NewTagDatabase()

	// Test with an empty database
	if len(db.GetAllTags()) != 0 {
		t.Error("GetAllTags() on an empty database should return an empty slice")
	}

	// Populate the database
	tag1 := &Tag{Name: "TagA", TypeInfo: &TypeInfo{DataType: TypeREAL}}
	tag2 := &Tag{Name: "TagB", TypeInfo: &TypeInfo{DataType: TypeSTRING}}
	_ = db.AddTag(tag1)
	_ = db.AddTag(tag2)

	allTags := db.GetAllTags()
	if len(allTags) != 2 {
		t.Fatalf("GetAllTags() returned %d tags, want 2", len(allTags))
	}

	// Sort for predictable comparison
	sort.Slice(allTags, func(i, j int) bool {
		return allTags[i].Name < allTags[j].Name
	})

	if allTags[0].Name != "TagA" || allTags[1].Name != "TagB" {
		t.Errorf("GetAllTags() returned incorrect or unordered tags. Got %s and %s", allTags[0].Name, allTags[1].Name)
	}
}

// TestGetAllTagNames verifies that all tag names can be retrieved correctly.
func TestGetAllTagNames(t *testing.T) {
	db := NewTagDatabase()

	// 1. Test with an empty database
	names := db.GetAllTagNames()
	if len(names) != 0 {
		t.Errorf("GetAllTagNames() on an empty database should return an empty slice, but got %d elements", len(names))
	}

	// 2. Populate the database
	tag1 := &Tag{Name: "TagB", TypeInfo: &TypeInfo{DataType: TypeREAL}}
	tag2 := &Tag{Name: "TagA", TypeInfo: &TypeInfo{DataType: TypeSTRING}}
	_ = db.AddTag(tag1)
	_ = db.AddTag(tag2)

	allNames := db.GetAllTagNames()
	if len(allNames) != 2 {
		t.Fatalf("GetAllTagNames() returned %d names, want 2", len(allNames))
	}

	// Sort for predictable comparison, as map iteration order is not guaranteed.
	sort.Strings(allNames)
	expectedNames := []string{"TagA", "TagB"}
	if allNames[0] != expectedNames[0] || allNames[1] != expectedNames[1] {
		t.Errorf("GetAllTagNames() returned incorrect names. Got %v, want %v", allNames, expectedNames)
	}
}

// TestGetTags verifies retrieving multiple tags at once.
func TestGetTags(t *testing.T) {
	db := NewTagDatabase()

	// Add some tags
	tag1 := &Tag{Name: "Tag1", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(1)}
	tag2 := &Tag{Name: "Tag2", TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(2.0)}
	tag3 := &Tag{Name: "Tag3", TypeInfo: &TypeInfo{DataType: TypeBOOL}, Value: plc.BOOL(true)}
	_ = db.AddTag(tag1)
	_ = db.AddTag(tag2)
	_ = db.AddTag(tag3)

	// Request two existing tags and one non-existent tag
	namesToGet := []string{"Tag1", "Tag3", "NonExistentTag"}
	foundTags := db.GetTags(namesToGet)

	// 1. Check the number of tags returned
	if len(foundTags) != 2 {
		t.Fatalf("GetTags() returned %d tags, want 2", len(foundTags))
	}

	// 2. Verify the correct tags were returned
	if _, ok := foundTags["Tag1"]; !ok {
		t.Error("GetTags() did not return 'Tag1'")
	}
	if _, ok := foundTags["Tag3"]; !ok {
		t.Error("GetTags() did not return 'Tag3'")
	}

	// 3. Verify a non-existent tag was not returned
	if _, ok := foundTags["NonExistentTag"]; ok {
		t.Error("GetTags() should not have returned 'NonExistentTag'")
	}
}

// TestGetTagsByType verifies retrieving tags by their data type.
func TestGetTagsByType(t *testing.T) {
	db := NewTagDatabase()

	// Add some tags with different types
	tag1 := &Tag{Name: "TagDINT1", TypeInfo: &TypeInfo{DataType: TypeDINT}}
	tag2 := &Tag{Name: "TagREAL1", TypeInfo: &TypeInfo{DataType: TypeREAL}}
	tag3 := &Tag{Name: "TagDINT2", TypeInfo: &TypeInfo{DataType: TypeDINT}}
	tag4 := &Tag{Name: "TagSTRING1", TypeInfo: &TypeInfo{DataType: TypeSTRING}}
	_ = db.AddTag(tag1)
	_ = db.AddTag(tag2)
	_ = db.AddTag(tag3)
	_ = db.AddTag(tag4)

	// 1. Test retrieving DINT tags
	dintTags := db.GetTagsByType(TypeDINT)
	if len(dintTags) != 2 {
		t.Fatalf("GetTagsByType(TypeDINT) returned %d tags, want 2", len(dintTags))
	}

	// Verify the correct tags were returned
	foundDINT1 := false
	foundDINT2 := false
	for _, tag := range dintTags {
		if tag.TypeInfo.DataType != TypeDINT {
			t.Errorf("GetTagsByType(TypeDINT) returned a tag with wrong type: %s", tag.TypeInfo.DataType)
		}
		if tag.Name == "TagDINT1" {
			foundDINT1 = true
		}
		if tag.Name == "TagDINT2" {
			foundDINT2 = true
		}
	}
	if !foundDINT1 || !foundDINT2 {
		t.Error("GetTagsByType(TypeDINT) did not return all expected tags.")
	}

	// 2. Test retrieving a type with no tags
	lintTags := db.GetTagsByType(TypeLINT)
	if len(lintTags) != 0 {
		t.Errorf("GetTagsByType(TypeLINT) should have returned an empty slice, but got %d elements", len(lintTags))
	}
}

// TestTagDatabaseConcurrency ensures the database is thread-safe.
func TestTagDatabaseConcurrency(t *testing.T) {
	db := NewTagDatabase()
	var wg sync.WaitGroup
	numGoroutines := 100

	// Concurrently add tags
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tagName := fmt.Sprintf("ConcurrentTag_%d", i) // Corrected from TypeINT to TypeINT
			tag := &Tag{Name: tagName, TypeInfo: &TypeInfo{DataType: TypeINT}}
			_ = db.AddTag(tag) // Ignore errors for this test, focusing on race conditions
			_, _ = db.GetTag(tagName)
		}(i)
	}

	wg.Wait()

	// Final check
	allTags := db.GetAllTags()
	if len(allTags) != numGoroutines {
		t.Errorf("After concurrent adds, expected %d tags, but got %d", numGoroutines, len(allTags))
	}
}

// TestPopulateDatabaseFromVariables verifies that the database is correctly populated from the global variables.
func TestPopulateDatabaseFromVariables(t *testing.T) {
	db := NewTagDatabase()
	err := PopulateDatabaseFromVariables(db)
	if err != nil {
		t.Fatalf("PopulateDatabaseFromVariables() returned an unexpected error: %v", err)
	}

	// Check for a few specific tags to ensure they were created correctly.
	testCases := []struct {
		tagName             string
		elementIndex        int
		expectedType        DataType
		expectedElementType DataType
		expectedDirectAddr  string
	}{
		{"I.B", 0, TypeARRAY, TypeBOOL, "%IX0.0"},
		{"I.B", 9, TypeARRAY, TypeBOOL, "%IX1.1"},
		{"Q.R", 0, TypeARRAY, TypeREAL, "%QD0"},
		{"M.W", 0, TypeARRAY, TypeWORD, "%MW0"},
		{"M.W", 5, TypeARRAY, TypeWORD, "%MW10"},
	}

	for _, tc := range testCases {
		t.Run(tc.expectedDirectAddr, func(t *testing.T) {
			tag, found := db.GetTag(tc.tagName)
			if !found {
				t.Fatalf("Tag '%s' was not found in the database", tc.tagName)
			}
			if tag.TypeInfo.DataType != tc.expectedType {
				t.Errorf("Tag '%s' has wrong DataType. Got %s, want %s", tc.tagName, tag.TypeInfo.DataType, tc.expectedType)
			}
			if tag.TypeInfo.ElementType != tc.expectedElementType {
				t.Errorf("Tag '%s' has wrong ElementType. Got %s, want %s", tc.tagName, tag.TypeInfo.ElementType, tc.expectedElementType)
			}
			if tag.DirectAddress != "" { // The base array tag itself might not have a direct address
				t.Logf("Note: Direct address on base tag '%s' is '%s'", tc.tagName, tag.DirectAddress)
			}
		})
	}

	// Ensure non-array fields were not added.
	_, found := db.GetTag("I.T")
	if found {
		t.Error("Tag 'I.T' should not have been created as it is not an array field")
	}
}

// TestTaggerInterfaceImplementation verifies that the Tag struct correctly implements the Tagger interface.
func TestTaggerInterfaceImplementation(t *testing.T) {
	tag := &Tag{
		Name:  "MyTag",
		Alias: "MyAlias",
		TypeInfo: &TypeInfo{
			DataType: TypeLREAL,
		},
		Description: "A sample description.",
		Forced:      false,
	}

	// Assign to the interface to check for compile-time satisfaction.
	var _ Tagger = tag

	// When Alias is set, GetName() should return the Alias.
	if tag.GetName() != "MyAlias" {
		t.Errorf("GetName() with alias = %s; want 'MyAlias'", tag.GetName())
	}

	// When Alias is not set, GetName() should return the Name.
	tag.Alias = ""
	if tag.GetName() != "MyTag" {
		t.Errorf("GetName() without alias = %s; want 'MyTag'", tag.GetName())
	}
	tag.Alias = "MyAlias" // Reset for next check

	if tag.GetAlias() != "MyAlias" {
		t.Errorf("GetAlias() = %s; want 'MyAlias'", tag.GetAlias())
	}
	if tag.GetDataType() != TypeLREAL {
		t.Errorf("GetDataType() = %s; want '%s'", tag.GetDataType(), TypeLREAL)
	}
	if tag.GetDescription() != "A sample description." {
		t.Errorf("GetDescription() = %s; want 'A sample description.'", tag.GetDescription())
	}
	if tag.IsForced() != false {
		t.Errorf("IsForced() with Forced false = %v; want false", tag.IsForced())
	}

	// Test with a true Forced flag
	tag.Forced = true
	if tag.IsForced() != true {
		t.Errorf("IsForced() with Forced true = %v; want true", tag.IsForced())
	}
}

// PrintTagDetails is a helper function for the example below. It accepts any
// type that satisfies the Tagger interface.
func PrintTagDetails(tag Tagger) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Name: %s", tag.GetName()))
	builder.WriteString(fmt.Sprintf(", Alias: %s", tag.GetAlias()))
	builder.WriteString(fmt.Sprintf(", DataType: %s", tag.GetDataType()))
	builder.WriteString(fmt.Sprintf(", Value: %v", tag.GetValue()))
	builder.WriteString(fmt.Sprintf(", Forced: %v", tag.IsForced()))
	return builder.String()
}

// TestTaggerInterfaceUsage demonstrates how a function can accept the Tagger
// interface to work with any tag-like object.
func TestTaggerInterfaceUsage(t *testing.T) {
	// 1. Create an instance of a struct that implements the Tagger interface.
	//    In this case, we use the `Tag` struct we've already defined.
	myTag := &Tag{
		Name:  "Motor.Speed",
		Alias: "MTR_SPD",
		TypeInfo: &TypeInfo{
			DataType: TypeREAL,
		},
		Value:       plc.REAL(1500.0),
		Description: "Current speed of the main motor in RPM.",
		ForceValue:  plc.REAL(0.0),
		Forced:      true, // The tag is forced.
	}

	// 2. Pass the concrete type (*Tag) to a function that expects the
	//    interface (Tagger). This works because *Tag has all the methods
	//    required by the Tagger interface.
	details := PrintTagDetails(myTag)

	// 3. Verify the output.
	expected := "Name: MTR_SPD, Alias: MTR_SPD, DataType: REAL, Value: 0, Forced: true"
	if details != expected {
		t.Errorf("PrintTagDetails output was incorrect.\nGot:  %s\nWant: %s", details, expected)
	}

	t.Log("Successfully demonstrated passing a concrete type (*Tag) to a function expecting an interface (Tagger).")
	t.Logf("Output of PrintTagDetails: %s", details)
}

func TestGetAndSetTagValue(t *testing.T) {
	db := NewTagDatabase()
	tagName := "MyTestTag"
	initialTag := &Tag{
		Name: tagName,
		TypeInfo: &TypeInfo{
			DataType: TypeDINT,
		},
		Value: plc.DINT(100),
	}
	db.AddTag(initialTag)

	// 1. Test GetTagValue
	val, err := db.GetTagValue(tagName)
	if err != nil {
		t.Fatalf("GetTagValue returned an unexpected error: %v", err)
	}
	if val != plc.DINT(100) {
		t.Errorf("GetTagValue returned %v, want %v", val, plc.DINT(100))
	}

	// 2. Test SetTagValue with correct type
	err = db.SetTagValue(tagName, plc.DINT(200))
	if err != nil {
		t.Fatalf("SetTagValue returned an unexpected error: %v", err)
	}

	// Verify the value was updated
	updatedVal, _ := db.GetTagValue(tagName)
	if updatedVal != plc.DINT(200) {
		t.Errorf("Value after SetTagValue is %v, want %v", updatedVal, plc.DINT(200))
	}

	// 3. Test GetValue method on the Tag struct itself
	tag, _ := db.GetTag(tagName)
	if tag.GetValue() != plc.DINT(200) {
		t.Errorf("tag.GetValue() returned %v, want %v", tag.GetValue(), plc.DINT(200))
	}
}

// TestTagGetValueForced verifies that GetValue returns the ForceValue when a tag is forced.
func TestTagGetValueForced(t *testing.T) {
	// 1. Create a tag that is forced.
	forcedTag := &Tag{
		Name: "ForcedTag",
		TypeInfo: &TypeInfo{
			DataType: TypeDINT,
		},
		Value:      plc.DINT(100),
		Forced:     true,
		ForceValue: plc.DINT(999),
	}

	// 2. Call GetValue and check if it returns the ForceValue.
	val := forcedTag.GetValue()
	if val != plc.DINT(999) {
		t.Errorf("GetValue() on a forced tag should return ForceValue. Got %v, want %v", val, plc.DINT(999))
	}

	// 3. Create a tag that is NOT forced.
	notForcedTag := &Tag{
		Name: "NotForcedTag",
		TypeInfo: &TypeInfo{
			DataType: TypeDINT,
		},
		Value:      plc.DINT(100),
		Forced:     false,
		ForceValue: plc.DINT(999), // ForceValue is set but should be ignored.
	}

	// 4. Call GetValue and check if it returns the regular Value.
	val = notForcedTag.GetValue()
	if val != plc.DINT(100) {
		t.Errorf("GetValue() on a non-forced tag should return Value. Got %v, want %v", val, plc.DINT(100))
	}
}

// TestSetTagValue_Errors checks error conditions for SetTagValue.
func TestSetTagValue_Errors(t *testing.T) {
	db := NewTagDatabase()
	tagName := "MyTag"
	db.AddTag(&Tag{Name: tagName, TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(1.23)})

	// 1. Test setting a non-existent tag
	err := db.SetTagValue("NonExistentTag", plc.REAL(4.56))
	if err == nil {
		t.Error("SetTagValue should have returned an error for a non-existent tag")
	}

	// 2. Test setting a value with the wrong type
	err = db.SetTagValue(tagName, plc.DINT(123))
	if err == nil {
		t.Error("SetTagValue should have returned a type mismatch error")
	}
	expectedError := "type mismatch for tag 'MyTag': expects DataType REAL, but got DINT"
	if err.Error() != expectedError {
		t.Errorf("SetTagValue returned wrong error message.\nGot:  %s\nWant: %s", err.Error(), expectedError)
	}

	// 3. Test setting a value with an unsupported type
	type UnsupportedType struct{}
	err = db.SetTagValue(tagName, UnsupportedType{})
	if err == nil {
		t.Error("SetTagValue should have returned an unsupported type error")
	}

	// Verify the original value was not changed after errors
	val, _ := db.GetTagValue(tagName)
	if val != plc.REAL(1.23) {
		t.Errorf("Tag value was modified after an error. Got %v, want %v", val, plc.REAL(1.23))
	}
}

// TestGetTagValue_Error checks error conditions for GetTagValue.
func TestGetTagValue_Error(t *testing.T) {
	db := NewTagDatabase()

	// Test getting a non-existent tag
	_, err := db.GetTagValue("NonExistentTag")
	if err == nil {
		t.Error("GetTagValue should have returned an error for a non-existent tag")
	}
}

// TestSetTagDescription verifies the SetTagDescription method.
func TestSetTagDescription(t *testing.T) {
	db := NewTagDatabase()
	tagName := "MyDescribedTag"
	initialDescription := "Initial description."

	// Add a tag with an initial description.
	db.AddTag(&Tag{Name: tagName, TypeInfo: &TypeInfo{DataType: TypeSTRING}, Description: initialDescription})

	// 1. Update the description.
	newDescription := "This is the updated description."
	updatedTag, err := db.SetTagDescription(tagName, newDescription)
	if err != nil {
		t.Fatalf("SetTagDescription returned an unexpected error: %v", err)
	}

	// Verify the change.
	if updatedTag.Description != newDescription {
		t.Errorf("Returned tag description was not updated. Got '%s', want '%s'", updatedTag.Description, newDescription)
	}
	retrievedDesc, _ := db.GetTagDescription(tagName)
	if retrievedDesc != newDescription {
		t.Errorf("GetTagDescription() did not return the updated value. Got '%s', want '%s'", retrievedDesc, newDescription)
	}

	// 2. Test error on non-existent tag.
	_, err = db.SetTagDescription("NonExistentTag", "some description")
	if err == nil {
		t.Error("SetTagDescription should have returned an error for a non-existent tag.")
	}
}

// TestSetTagAlias verifies the SetTagAlias method.
func TestSetTagAlias(t *testing.T) {
	db := NewTagDatabase()
	tagName := "MyAliasedTag"

	// Add a tag with no initial alias.
	db.AddTag(&Tag{Name: tagName, TypeInfo: &TypeInfo{DataType: TypeDINT}})

	// 1. Update the alias.
	newAlias := "TheNewAlias"
	err := db.SetTagAlias(tagName, newAlias)
	if err != nil {
		t.Fatalf("SetTagAlias returned an unexpected error: %v", err)
	}

	// Verify the change.
	tag, _ := db.GetTag(tagName)
	if tag.Alias != newAlias {
		t.Errorf("Tag alias was not updated. Got '%s', want '%s'", tag.Alias, newAlias)
	}
	// Remember that GetName() should now return the alias.
	if tag.GetName() != newAlias {
		t.Errorf("tag.GetName() did not return the new alias. Got '%s', want '%s'", tag.GetName(), newAlias)
	}
	if tag.GetAlias() != newAlias {
		t.Errorf("tag.GetAlias() did not return the new alias. Got '%s', want '%s'", tag.GetAlias(), newAlias)
	}

	// 2. Test error on non-existent tag.
	err = db.SetTagAlias("NonExistentTag", "some-alias")
	if err == nil {
		t.Error("SetTagAlias should have returned an error for a non-existent tag.")
	}
}

// TestGetAndSetTagForced verifies the SetTagForced and GetTagForced methods.
func TestGetAndSetTagForced(t *testing.T) {
	db := NewTagDatabase()
	tagName := "MyForcedTag"

	// Add a tag, initially not forced.
	db.AddTag(&Tag{Name: tagName, TypeInfo: &TypeInfo{DataType: TypeBOOL}, Forced: false})

	// 1. Set Forced to true.
	updatedTag, err := db.SetTagForced(tagName, true)
	if err != nil {
		t.Fatalf("SetTagForced(true) returned an unexpected error: %v", err)
	}
	if !updatedTag.Forced {
		t.Error("Returned tag from SetTagForced(true) was not marked as forced.")
	}

	// Verify the change using GetTagForced.
	forced, err := db.GetTagForced(tagName)
	if err != nil {
		t.Fatalf("GetTagForced() returned an unexpected error: %v", err)
	}
	if !forced {
		t.Error("Tag should be forced after setting to true, but it's not.")
	}

	// 2. Set Forced back to false.
	updatedTag, err = db.SetTagForced(tagName, false)
	if err != nil {
		t.Fatalf("SetTagForced(false) returned an unexpected error: %v", err)
	}
	if updatedTag.Forced {
		t.Error("Returned tag from SetTagForced(false) was still marked as forced.")
	}

	// Verify the change.
	forced, _ = db.GetTagForced(tagName)
	if forced {
		t.Error("Tag should not be forced after setting to false, but it is.")
	}

	// 3. Test error on non-existent tag.
	_, err = db.SetTagForced("NonExistentTag", true)
	if err == nil {
		t.Error("GetTagForced should have returned an error for a non-existent tag.")
	}
}

// TestGetAndSetTagForceValue verifies the SetTagForceValue and GetTagForceValue methods.
func TestGetAndSetTagForceValue(t *testing.T) {
	db := NewTagDatabase()
	tagName := "MyForceValueTag"

	// Add a tag.
	db.AddTag(&Tag{Name: tagName, TypeInfo: &TypeInfo{DataType: TypeDINT}})

	// 1. Set a valid force value.
	forceValue := plc.DINT(888)
	updatedTag, err := db.SetTagForceValue(tagName, forceValue)
	if err != nil {
		t.Fatalf("SetTagForceValue returned an unexpected error: %v", err)
	}
	if updatedTag.ForceValue != forceValue {
		t.Errorf("Returned tag from SetTagForceValue has incorrect ForceValue. Got %v, want %v", updatedTag.ForceValue, forceValue)
	}

	// Verify the change using GetTagForceValue.
	retrievedValue, err := db.GetTagForceValue(tagName)
	if err != nil {
		t.Fatalf("GetTagForceValue() returned an unexpected error: %v", err)
	}
	if retrievedValue != forceValue {
		t.Errorf("GetTagForceValue() returned %v, want %v", retrievedValue, forceValue)
	}

	// 2. Attempt to set a value with the wrong type.
	_, err = db.SetTagForceValue(tagName, plc.REAL(1.23))
	if err == nil {
		t.Error("SetTagForceValue should have returned a type mismatch error.")
	}

	// Verify the force value was not changed.
	retrievedValue, _ = db.GetTagForceValue(tagName)
	if retrievedValue != forceValue {
		t.Errorf("Force value was modified after a type mismatch error. Got %v, want %v", retrievedValue, forceValue)
	}

	// 3. Clear the force value by setting it to nil.
	_, err = db.SetTagForceValue(tagName, nil)
	if err != nil {
		t.Fatalf("SetTagForceValue(nil) returned an unexpected error: %v", err)
	}
	retrievedValue, _ = db.GetTagForceValue(tagName)
	if retrievedValue != nil {
		t.Errorf("Force value should be nil after setting to nil, but got %v", retrievedValue)
	}

	// 4. Test error on non-existent tag.
	_, err = db.SetTagForceValue("NonExistentTag", plc.DINT(1))
	if err == nil {
		t.Error("GetTagForceValue should have returned an error for a non-existent tag.")
	}
}

// TestGetTagAlias verifies the GetTagAlias method.
func TestGetTagAlias(t *testing.T) {
	db := NewTagDatabase()
	tagName := "TagWithAlias"
	alias := "MyAlias"

	// Add a tag with an alias.
	db.AddTag(&Tag{Name: tagName, TypeInfo: &TypeInfo{DataType: TypeDINT}, Alias: alias})

	// 1. Retrieve the alias.
	retrievedAlias, err := db.GetTagAlias(tagName)
	if err != nil {
		t.Fatalf("GetTagAlias returned an unexpected error: %v", err)
	}
	if retrievedAlias != alias {
		t.Errorf("GetTagAlias() returned '%s', want '%s'", retrievedAlias, alias)
	}

	// 2. Test error on non-existent tag.
	_, err = db.GetTagAlias("NonExistentTag")
	if err == nil {
		t.Error("GetTagAlias should have returned an error for a non-existent tag.")
	}
}

// TestGetTagDescription verifies the GetTagDescription method.
func TestGetTagDescription(t *testing.T) {
	db := NewTagDatabase()
	tagName := "TagWithDescription"
	description := "This is a test description."

	// Add a tag with a description.
	db.AddTag(&Tag{Name: tagName, TypeInfo: &TypeInfo{DataType: TypeSTRING}, Description: description})

	// 1. Retrieve the description.
	retrievedDesc, err := db.GetTagDescription(tagName)
	if err != nil {
		t.Fatalf("GetTagDescription returned an unexpected error: %v", err)
	}
	if retrievedDesc != description {
		t.Errorf("GetTagDescription() returned '%s', want '%s'", retrievedDesc, description)
	}

	// 2. Test error on non-existent tag.
	_, err = db.GetTagDescription("NonExistentTag")
	if err == nil {
		t.Error("GetTagDescription should have returned an error for a non-existent tag.")
	}
}

// TestRenameTag verifies the RenameTag method.
func TestRenameTag(t *testing.T) {
	db := NewTagDatabase()
	oldName := "OldTagName"
	newName := "NewTagName"
	existingName := "ExistingTag"

	db.AddTag(&Tag{Name: oldName, TypeInfo: &TypeInfo{DataType: TypeINT}, Value: plc.INT(123)})
	db.AddTag(&Tag{Name: existingName, TypeInfo: &TypeInfo{DataType: TypeBOOL}, Value: plc.BOOL(true)})

	// 1. Test successful rename.
	renamedTag, err := db.RenameTag(oldName, newName)
	if err != nil {
		t.Fatalf("RenameTag returned an unexpected error: %v", err)
	}

	// Verify the returned tag has the new name.
	if renamedTag.Name != newName {
		t.Errorf("Returned tag from RenameTag has wrong name. Got '%s', want '%s'", renamedTag.Name, newName)
	}

	// Verify the old tag is gone.
	_, found := db.GetTag(oldName)
	if found {
		t.Error("Old tag name should not exist after rename.")
	}

	// Verify the new tag exists and has the correct data.
	newTag, found := db.GetTag(newName)
	if !found {
		t.Fatal("New tag name should exist after rename.")
	}
	if newTag.Value != plc.INT(123) {
		t.Errorf("Renamed tag has wrong value. Got %v, want %v", newTag.Value, plc.INT(123))
	}
	if newTag.Name != newName {
		t.Errorf("Tag retrieved by new name has incorrect internal name field. Got '%s', want '%s'", newTag.Name, newName)
	}

	// 2. Test renaming to an already existing tag name.
	_, err = db.RenameTag(newName, existingName)
	if err == nil {
		t.Fatal("RenameTag should have returned an error when renaming to an existing tag name.")
	}
	expectedError := fmt.Sprintf("cannot rename to '%s', a tag with that name already exists", existingName)
	if err.Error() != expectedError {
		t.Errorf("RenameTag returned wrong error message.\nGot:  %s\nWant: %s", err.Error(), expectedError)
	}

	// Verify the tag was not renamed.
	_, found = db.GetTag(newName)
	if !found {
		t.Error("Tag should not have been renamed after a collision error.")
	}

	// 3. Test renaming a non-existent tag.
	_, err = db.RenameTag("NonExistentTag", "SomeOtherName")
	if err == nil {
		t.Error("RenameTag should have returned an error when trying to rename a non-existent tag.")
	}
}

// TestRemoveTag verifies the RemoveTag method.
func TestRemoveTag(t *testing.T) {
	db := NewTagDatabase()
	tagToRemove := "TagToRemove"
	tagToKeep := "TagToKeep"

	db.AddTag(&Tag{Name: tagToRemove, TypeInfo: &TypeInfo{DataType: TypeINT}})
	db.AddTag(&Tag{Name: tagToKeep, TypeInfo: &TypeInfo{DataType: TypeBOOL}})

	// 1. Test successful removal.
	err := db.RemoveTag(tagToRemove)
	if err != nil {
		t.Fatalf("RemoveTag returned an unexpected error: %v", err)
	}

	// Verify the tag is gone.
	_, found := db.GetTag(tagToRemove)
	if found {
		t.Error("Tag should have been removed, but it was found.")
	}

	// Verify other tags are unaffected.
	_, found = db.GetTag(tagToKeep)
	if !found {
		t.Error("RemoveTag should not affect other tags, but a tag was removed.")
	}
	if count := len(db.GetAllTags()); count != 1 {
		t.Errorf("Expected 1 tag after removal, but got %d", count)
	}

	// 2. Test removing a non-existent tag.
	err = db.RemoveTag("NonExistentTag")
	if err == nil {
		t.Error("RemoveTag should have returned an error for a non-existent tag.")
	}
}

// benchmarkDB is a helper to create a pre-populated database for benchmarks.
func benchmarkDB(b *testing.B) (*TagDatabase, string) {
	b.Helper()
	db := NewTagDatabase()
	tagName := "BenchmarkTag"
	tag := &Tag{
		Name:        tagName,
		Alias:       "BenchAlias",
		Description: "A very long description for the benchmark tag to ensure there is enough data to copy.",
		TypeInfo: &TypeInfo{
			DataType: TypeLREAL,
		},
		Value:      plc.LREAL(123.456),
		Forced:     true,
		ForceValue: plc.LREAL(789.012),
	}
	if err := db.AddTag(tag); err != nil {
		b.Fatalf("Failed to add tag for benchmark: %v", err)
	}
	return db, tagName
}

// BenchmarkGetTag measures the performance of retrieving the entire Tag struct.
func BenchmarkGetTag(b *testing.B) {
	db, tagName := benchmarkDB(b)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// The result is intentionally not used to focus on the retrieval cost.
		_, _ = db.GetTag(tagName)
	}
}

// BenchmarkGetTagValue measures the performance of retrieving only the tag's value.
func BenchmarkGetTagValue(b *testing.B) {
	db, tagName := benchmarkDB(b)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = db.GetTagValue(tagName)
	}
}

// BenchmarkGetTagAlias measures the performance of retrieving only the tag's alias.
func BenchmarkGetTagAlias(b *testing.B) {
	db, tagName := benchmarkDB(b)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = db.GetTagAlias(tagName)
	}
}

// BenchmarkGetTagDescription measures the performance of retrieving only the tag's description.
func BenchmarkGetTagDescription(b *testing.B) {
	db, tagName := benchmarkDB(b)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = db.GetTagDescription(tagName)
	}
}

// TestWriteAndReadTags verifies the entire persistence cycle.
func TestWriteAndReadTags(t *testing.T) {
	// --- 1. Setup and Write Phase ---
	dbWrite := NewTagDatabase()
	//tempDir := t.TempDir()
	//filePath := filepath.Join(tempDir, "persistent_tags.txt")
	filePath := "persistent_tags.txt"

	// Add a mix of tags; all should be persisted.
	tagsToWrite := []Tag{
		{Name: "TagDINT", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(123)},
		{Name: "TagREAL", TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(45.67)},
		{Name: "TagINT", TypeInfo: &TypeInfo{DataType: TypeINT}, Value: plc.INT(999)},
		{Name: "TagSTRING", TypeInfo: &TypeInfo{DataType: TypeSTRING}, Value: plc.STRING("hello world")},
	}
	for _, tag := range tagsToWrite {
		if err := dbWrite.AddTag(&tag); err != nil { // Pass by pointer
			t.Fatalf("Failed to add tag %s during write setup: %v", tag.Name, err)
		}
	}

	// Write the tags to the file
	if err := dbWrite.WriteTagsToFile(filePath); err != nil {
		t.Fatalf("WriteTagsToFile() returned an unexpected error: %v", err)
	}

	// Verify the file content
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read the created persistence file: %v", err)
	}

	// Since map iteration order is not guaranteed, we check for the presence of all expected lines.
	expectedLines := map[string]bool{
		"TagDINT=123":           true,
		"TagREAL=45.67":         true,
		"TagINT=999":            true,
		"TagSTRING=hello world": true,
	}
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		if line != "" && !expectedLines[line] {
			t.Errorf("Unexpected line in output file: %s", line)
		}
	}

	// --- 2. Read and Verify Phase ---
	dbRead := NewTagDatabase()

	// Populate the "new" database, simulating a restart with default values
	tagsToRead := []Tag{
		{Name: "TagDINT", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(0)},
		{Name: "TagREAL", TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(0.0)},
		{Name: "TagINT", TypeInfo: &TypeInfo{DataType: TypeINT}, Value: plc.INT(0)},
		{Name: "TagSTRING", TypeInfo: &TypeInfo{DataType: TypeSTRING}, Value: plc.STRING("")},
		{Name: "UntouchedTag", TypeInfo: &TypeInfo{DataType: TypeBOOL}, Value: plc.BOOL(true)}, // This tag is not in the file
	}
	for _, tag := range tagsToRead {
		if err := dbRead.AddTag(&tag); err != nil { // Pass by pointer
			t.Fatalf("Failed to add tag %s during read setup: %v", tag.Name, err)
		}
	}

	// Read the values back from the file
	if err := dbRead.ReadTagsFromFile(filePath); err != nil {
		t.Fatalf("ReadTagsFromFile() returned an unexpected error: %v", err)
	}

	// Verify the values in the new database
	testCases := []struct {
		tagName     string
		expectedVal interface{}
	}{
		{"TagDINT", plc.DINT(123)},
		{"TagREAL", plc.REAL(45.67)},
		{"TagINT", plc.INT(999)},
		{"TagSTRING", plc.STRING("hello world")},
		{"UntouchedTag", plc.BOOL(true)}, // Should remain unchanged
	}

	for _, tc := range testCases {
		t.Run("Verify_"+tc.tagName, func(t *testing.T) {
			val, err := dbRead.GetTagValue(tc.tagName)
			if err != nil {
				t.Fatalf("GetTagValue failed for %s: %v", tc.tagName, err)
			}
			if val != tc.expectedVal {
				t.Errorf("Tag %s has wrong value. Got %v (%T), want %v (%T)", tc.tagName, val, val, tc.expectedVal, tc.expectedVal)
			}
		})
	}
	// Ensure the test cleans up the created file.
	t.Cleanup(func() { os.Remove(filePath) })
}

// TestReadTags_FileNotExist tests that no error occurs if the file doesn't exist.
func TestReadTags_FileNotExist(t *testing.T) {
	db := NewTagDatabase()
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "non_existent_file.txt")

	err := db.ReadTagsFromFile(filePath)
	if err != nil {
		t.Fatalf("ReadTagsFromFile() should not return an error for a non-existent file, but got: %v", err)
	}
}

// TestReadTags_ParseError tests that the function continues after a parsing error.
func TestReadTags_ParseError(t *testing.T) {
	db := NewTagDatabase()
	db.AddTag(&Tag{Name: "MyTag", TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(0)})

	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "bad_file.txt")

	// Create a file with a malformed line
	badContent := []byte("MyTag=not_a_number")
	if err := os.WriteFile(filePath, badContent, 0666); err != nil {
		t.Fatalf("Failed to write bad file: %v", err)
	}

	err := db.ReadTagsFromFile(filePath)
	if err == nil {
		t.Fatal("ReadTagsFromFile() should have returned an error for a parse failure")
	}
	if !strings.Contains(err.Error(), "error parsing value for tag 'MyTag'") {
		t.Errorf("Expected a parsing error, but got: %v", err)
	}
}

// MotorData is an example of a User-Defined Type (UDT).
// It's a struct that will be used as a tag's value.
type MotorData struct {
	Speed    plc.REAL // Corrected from TypeREAL to TypeREAL
	Current  plc.REAL
	Temp     plc.REAL
	Running  plc.BOOL
	Tripped  plc.BOOL
	TestName string
}

// TypeName implements the UDT interface, returning the unique name for this type.
func (m *MotorData) TypeName() DataType {
	return "MotorData"
}

// TestUDTPersistence verifies that a UDT can be added, persisted to a file, and read back correctly.
func TestUDTPersistence(t *testing.T) {
	// 1. Register our new UDT so the system knows about it.
	RegisterUDT(&MotorData{})

	// --- Write Phase ---
	dbWrite := NewTagDatabase()
	//tempDir := t.TempDir()
	//filePath := filepath.Join(tempDir, "udt_persistence.txt")
	filePath := "udt_persistence.txt"

	// 2. Create an instance of our UDT tag.
	motorTag := Tag{
		Name: "MainMotor",
		TypeInfo: &TypeInfo{
			DataType: "MotorData", // The DataType string must match TypeName()
		},
		Value: &MotorData{
			Speed:   1750.5,
			Current: 45.2,
			Running: true,
		},
	}

	if err := dbWrite.AddTag(&motorTag); err != nil {
		t.Fatalf("Failed to add UDT tag: %v", err)
	}

	// 3. Write the database state to a file using the test helper.
	err1 := dbWrite.WriteTagsToFile(filePath)
	if err1 != nil {
		t.Fatalf("Failed to write UDT to file: %v", err1)
	}

	// --- Read Phase ---
	dbRead := NewTagDatabase()
	// 4. Pre-populate the read DB with a placeholder for the tag. This is crucial
	// so the read function knows the expected Type.
	dbRead.AddTag(&Tag{Name: "MainMotor", TypeInfo: &TypeInfo{DataType: "MotorData"}})

	// 5. Read from the file, which should deserialize the JSON into the tag.
	err2 := dbRead.ReadTagsFromFile(filePath)
	if err2 != nil {
		t.Fatalf("Failed to read UDT from file: %v", err2)
	}

	// 6. Verify the data was loaded correctly.
	retrievedTag, found := dbRead.GetTag("MainMotor")
	if !found {
		t.Fatal("Failed to retrieve UDT tag after reading from file.")
	}

	retrievedMotorData, ok := retrievedTag.Value.(*MotorData)
	if !ok {
		t.Fatalf("Retrieved tag value is not of type *MotorData")
	}

	if retrievedMotorData.Speed != 1750.5 || !retrievedMotorData.Running {
		t.Errorf("Data mismatch after reading UDT from file. Got %+v", retrievedMotorData)
	}
	// Ensure the test cleans up the created file.
	t.Cleanup(func() { os.Remove(filePath) })

}

// TestGetNestedUDTField verifies that nested fields of a UDT can be accessed
// using dot notation (e.g., "MyTag.MyField").
func TestGetNestedUDTField(t *testing.T) {
	RegisterUDT(&MotorData{})
	db := NewTagDatabase()

	motorTag := &Tag{
		Name: "MainMotor",
		TypeInfo: &TypeInfo{
			DataType: "MotorData",
		},
		Value: &MotorData{
			Speed:   1800.0,
			Current: 50.5,
			Running: true,
		},
	}
	db.AddTag(motorTag)

	testCases := []struct {
		name         string
		nestedTag    string
		expectedVal  interface{} // Corrected from TypeREAL to TypeREAL
		expectFound  bool        // Corrected from TypeBOOL to TypeBOOL
		expectedType DataType
	}{
		{"Access REAL field", "MainMotor.Speed", plc.REAL(1800.0), true, TypeREAL},
		{"Access BOOL field", "MainMotor.Running", plc.BOOL(true), true, TypeBOOL},
		{"Access non-existent field", "MainMotor.NonExistent", nil, false, ""},
		{"Access field on non-existent tag", "FakeMotor.Speed", nil, false, ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test GetTagValue
			val, err := db.GetTagValue(tc.nestedTag)
			if tc.expectFound {
				if err != nil {
					t.Fatalf("GetTagValue(%q) returned unexpected error: %v", tc.nestedTag, err)
				}
				if val != tc.expectedVal {
					t.Errorf("GetTagValue(%q) = %v; want %v", tc.nestedTag, val, tc.expectedVal)
				}
			} else {
				if err == nil {
					t.Errorf("GetTagValue(%q) was expected to fail, but it succeeded.", tc.nestedTag)
				}
			}
		})
	}
}

// TestSetNestedUDTField verifies that nested fields of a UDT can be written to
// using dot notation via SetTagValue.
func TestSetNestedUDTField(t *testing.T) {
	RegisterUDT(&MotorData{})
	db := NewTagDatabase()

	motorTag := &Tag{
		Name: "MainMotor",
		TypeInfo: &TypeInfo{
			DataType: "MotorData",
		},
		Value: &MotorData{
			Speed:   1800.0,
			Running: false,
		},
	}
	db.AddTag(motorTag)

	// 1. Test successful write to a nested field.
	newSpeed := plc.REAL(1950.5)
	err := db.SetTagValue("MainMotor.Speed", newSpeed)
	if err != nil {
		t.Fatalf("SetTagValue on nested field returned unexpected error: %v", err)
	}

	// Verify the change by reading it back.
	val, err := db.GetTagValue("MainMotor.Speed")
	if err != nil {
		t.Fatalf("GetTagValue for nested field failed after write: %v", err)
	}
	if val != newSpeed {
		t.Errorf("Nested field value was not updated. Got %v, want %v", val, newSpeed)
	}

	// Also verify by getting the whole UDT.
	tag, _ := db.GetTag("MainMotor")
	motorData := tag.Value.(*MotorData)
	if motorData.Speed != newSpeed {
		t.Errorf("UDT struct in map was not updated. Got speed %v, want %v", motorData.Speed, newSpeed)
	}

	// 2. Test writing with an incorrect type.
	err = db.SetTagValue("MainMotor.Speed", plc.DINT(2000)) // REAL field, DINT value
	if err == nil {
		t.Error("SetTagValue with type mismatch should have returned an error.")
	}

	// 3. Test writing to a non-existent field.
	err = db.SetTagValue("MainMotor.NonExistentField", plc.REAL(1.0))
	if err == nil {
		t.Error("SetTagValue on a non-existent field should have returned an error.")
	}

	// 4. Test writing to a non-existent base tag.
	err = db.SetTagValue("FakeMotor.Speed", plc.REAL(1.0))
	if err == nil {
		t.Error("SetTagValue on a non-existent base tag should have returned an error.")
	}
}

// MotorConfig represents a nested UDT.
type MotorConfig struct {
	MaxSpeed plc.REAL
	RampTime plc.TIME
}

// TypeName implements the UDT interface for MotorConfig.
func (mc *MotorConfig) TypeName() DataType {
	return "MotorConfig"
}

// DriveSystem is a parent UDT that contains another UDT.
type DriveSystem struct {
	Name   plc.STRING   // Corrected from TypeSTRING to TypeSTRING
	Config *MotorConfig // Nested UDT
	Active plc.BOOL     // Corrected from TypeBOOL to TypeBOOL
}

// TypeName implements the UDT interface for DriveSystem.
func (ds *DriveSystem) TypeName() DataType {
	return "DriveSystem"
}

// TestNestedUDTPersistence verifies that a UDT containing another UDT
// can be persisted and loaded correctly.
func TestNestedUDTPersistence(t *testing.T) {
	// 1. Register all UDTs involved, both parent and nested.
	// This is a crucial step for the system to be aware of all custom types.
	RegisterUDT(&DriveSystem{})
	RegisterUDT(&MotorConfig{})

	// --- Write Phase ---
	dbWrite := NewTagDatabase()
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "nested_udt_persistence.txt")

	driveTag := Tag{
		Name: "MainDrive",
		TypeInfo: &TypeInfo{
			DataType: "DriveSystem",
		},
		Value: &DriveSystem{
			Name: "Conveyor 1",
			Config: &MotorConfig{
				MaxSpeed: 3600.0,
				RampTime: plc.TIME(5 * time.Second), // 5 seconds
			},
			Active: true,
		},
	}

	if err := dbWrite.AddTag(&driveTag); err != nil {
		t.Fatalf("Failed to add nested UDT tag: %v", err)
	}

	if err := dbWrite.WriteTagsToFile(filePath); err != nil {
		t.Fatalf("Failed to write nested UDT to file: %v", err)
	}

	// --- Read Phase ---
	dbRead := NewTagDatabase()
	dbRead.AddTag(&Tag{Name: "MainDrive", TypeInfo: &TypeInfo{DataType: "DriveSystem"}})

	if err := dbRead.ReadTagsFromFile(filePath); err != nil {
		t.Fatalf("Failed to read nested UDT from file: %v", err)
	}

	retrievedTag, _ := dbRead.GetTag("MainDrive")
	retrievedDrive, _ := retrievedTag.Value.(*DriveSystem)

	if retrievedDrive.Name != "Conveyor 1" || retrievedDrive.Config.MaxSpeed != 3600.0 {
		t.Errorf("Data mismatch after reading nested UDT from file. Got %+v", retrievedDrive)
	}
}

// Define multi-level UDTs at the package level so methods can be attached.
type Level3 struct { // Corrected from TypeDINT to TypeDINT
	Value plc.DINT
}

// TypeName implements the UDT interface for Level3.
func (l *Level3) TypeName() DataType { return "Level3" }

type Level2 struct { // Corrected from TypeSTRING to TypeSTRING
	L3   *Level3
	Name plc.STRING
}

// TypeName implements the UDT interface for Level2.
func (l *Level2) TypeName() DataType { return "Level2" }

type Level1 struct { // Corrected from TypeBOOL to TypeBOOL
	L2     *Level2
	Active plc.BOOL
}

// TypeName implements the UDT interface for Level1.
func (l *Level1) TypeName() DataType { return "Level1" }

// TestMultiLevelNestedUDTAccess verifies that fields nested multiple levels deep
// within UDTs can be read and written correctly.
func TestMultiLevelNestedUDTAccess(t *testing.T) {
	// 1. Register the UDTs for multi-level nesting.
	// The types are now defined at the package level.
	RegisterUDT(&Level1{})
	RegisterUDT(&Level2{})
	RegisterUDT(&Level3{})

	// 2. Setup database and tag.
	db := NewTagDatabase()
	topTag := &Tag{
		Name: "Top",
		TypeInfo: &TypeInfo{
			DataType: "Level1",
		},
		Value: &Level1{
			Active: true,
			L2: &Level2{
				Name: "Nested Level 2",
				L3: &Level3{
					Value: 42,
				},
			},
		},
	}
	if err := db.AddTag(topTag); err != nil {
		t.Fatalf("Failed to add multi-level UDT tag: %v", err)
	}

	// 3. Test GetTagValue for a multi-level field.
	t.Run("ReadMultiLevel", func(t *testing.T) {
		val, err := db.GetTagValue("Top.L2.L3.Value")
		if err != nil {
			t.Fatalf("GetTagValue for multi-level field failed: %v", err)
		}

		expected := plc.DINT(42)
		if val != expected {
			t.Errorf("multi-level GetTagValue returned %v; want %v", val, expected)
		}
	})

	// 4. Test SetTagValue for a multi-level field.
	t.Run("WriteMultiLevel", func(t *testing.T) {
		newValue := plc.DINT(99)
		err := db.SetTagValue("Top.L2.L3.Value", newValue)
		if err != nil {
			t.Fatalf("SetTagValue for multi-level field failed: %v", err)
		}

		// Verify the change by reading it back.
		val, err := db.GetTagValue("Top.L2.L3.Value")
		if err != nil {
			t.Fatalf("GetTagValue after set failed: %v", err)
		}
		if val != newValue {
			t.Errorf("multi-level field value was not updated. Got %v, want %v", val, newValue)
		}

		// Also verify by getting the whole UDT.
		tag, _ := db.GetTag("Top")
		level1 := tag.Value.(*Level1)
		if level1.L2.L3.Value != newValue {
			t.Errorf("UDT struct in map was not updated. Got value %v, want %v", level1.L2.L3.Value, newValue)
		}
	})
}

// BenchmarkGetNestedTagValue measures getting a value from a nested UDT field.
func BenchmarkGetNestedTagValue(b *testing.B) {
	RegisterUDT(&DriveSystem{})
	RegisterUDT(&MotorConfig{})
	db := NewTagDatabase()
	driveTag := &Tag{
		Name: "MainDrive",
		TypeInfo: &TypeInfo{
			DataType: "DriveSystem",
		},
		Value: &DriveSystem{
			Name:   "Conveyor 1",
			Config: &MotorConfig{MaxSpeed: 3600.0},
			Active: true,
		},
	}
	db.AddTag(driveTag)
	nestedTagName := "MainDrive.Config.MaxSpeed"

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = db.GetTagValue(nestedTagName)
	}
}

// BenchmarkSetNestedTagValue measures setting a value on a nested UDT field.
func BenchmarkSetNestedTagValue(b *testing.B) {
	RegisterUDT(&DriveSystem{})
	RegisterUDT(&MotorConfig{})
	db := NewTagDatabase()
	driveTag := &Tag{
		Name: "MainDrive",
		TypeInfo: &TypeInfo{
			DataType: "DriveSystem",
		},
		Value: &DriveSystem{
			Name:   "Conveyor 1",
			Config: &MotorConfig{MaxSpeed: 3600.0},
			Active: true,
		},
	}
	db.AddTag(driveTag)
	nestedTagName := "MainDrive.Config.MaxSpeed"
	newValue := plc.REAL(4000.0)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// We don't reset state for this benchmark to measure the raw cost of the
		// SetTagValue operation itself, including lock contention.
		_ = db.SetTagValue(nestedTagName, newValue)
	}
}

// BenchmarkGetNestedTag measures getting a temporary Tag struct for a nested UDT field.
func BenchmarkGetNestedTag(b *testing.B) {
	// The setup is identical to BenchmarkGetNestedTagValue, as GetTag is the underlying mechanism.
	// This benchmark specifically measures the cost of creating the temporary Tag struct.
	db, nestedTagName := setupNestedBenchmark(b)
	b.Run("GetNestedTag", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = db.GetTag(nestedTagName)
		}
	})
}

// setupNestedBenchmark is a helper to avoid code duplication in benchmarks.
func setupNestedBenchmark(b *testing.B) (*TagDatabase, string) {
	b.Helper()
	RegisterUDT(&DriveSystem{})
	RegisterUDT(&MotorConfig{})
	db := NewTagDatabase()
	driveTag := &Tag{
		Name: "MainDrive",
		TypeInfo: &TypeInfo{
			DataType: "DriveSystem",
		},
		Value: &DriveSystem{
			Name:   "Conveyor 1",
			Config: &MotorConfig{MaxSpeed: 3600.0},
			Active: true,
		},
	}
	db.AddTag(driveTag)
	nestedTagName := "MainDrive.Config.MaxSpeed"

	b.ReportAllocs()
	b.ResetTimer()
	return db, nestedTagName
}

// TestAddAndGetArrayTag tests adding and retrieving a native array tag.
func TestAddAndGetArrayTag(t *testing.T) {
	db := NewTagDatabase()
	tagName := "MyIntArray"

	// Create a slice of a specific PLC type.
	arrayValue := []plc.INT{10, 20, 30}

	// The DataType should be ARRAY, and ElementType should be INT.
	// The getDataType function will infer this from the value's type.
	dataType, _ := getDataType(reflect.TypeOf(arrayValue))
	if dataType != TypeARRAY {
		t.Fatalf("Expected data type to be ARRAY, but got %s", dataType)
	}

	tag := &Tag{
		Name:  tagName,
		Value: arrayValue,
		TypeInfo: &TypeInfo{
			DataType:    TypeARRAY,
			ElementType: TypeINT, // Explicitly set the element type
		},
	}

	if err := db.AddTag(tag); err != nil {
		t.Fatalf("Failed to add array tag: %v", err)
	}

	// Retrieve the whole array tag
	retrievedTag, found := db.GetTag(tagName)
	if !found {
		t.Fatal("Failed to retrieve array tag.")
	}

	if retrievedTag.TypeInfo.DataType != TypeARRAY {
		t.Errorf("Retrieved tag DataType should be ARRAY, got %s", retrievedTag.TypeInfo.DataType)
	}
	if retrievedTag.TypeInfo.ElementType != TypeINT {
		t.Errorf("Retrieved tag ElementType should be INT, got %s", retrievedTag.TypeInfo.ElementType)
	}

	// Check the value
	retrievedValue, ok := retrievedTag.Value.([]plc.INT)
	if !ok {
		t.Fatalf("Retrieved value is not of type []plc.INT, but %T", retrievedTag.Value)
	}
	if len(retrievedValue) != 3 || retrievedValue[1] != 20 {
		t.Errorf("Retrieved array value is incorrect. Got %v", retrievedValue)
	}
}

// TestArrayElementAccess verifies reading from and writing to individual array elements.
func TestArrayElementAccess(t *testing.T) {
	db := NewTagDatabase()
	tagName := "MyDintArray"
	arrayValue := []plc.DINT{100, 200, 300}

	tag := &Tag{
		Name:  tagName, // Corrected from TypeARRAY to TypeARRAY
		Value: arrayValue,
		TypeInfo: &TypeInfo{
			DataType:    TypeARRAY,
			ElementType: TypeDINT,
		},
	}
	db.AddTag(tag)

	// 1. Test reading an element
	elementName := "MyDintArray[1]"
	val, err := db.GetTagValue(elementName)
	if err != nil {
		t.Fatalf("GetTagValue for array element failed: %v", err)
	}
	if val != plc.DINT(200) {
		t.Errorf("Incorrect element value read. Got %v, want %v", val, plc.DINT(200))
	}

	// 2. Test writing to an element
	newValue := plc.DINT(250)
	err = db.SetTagValue(elementName, newValue)
	if err != nil {
		t.Fatalf("SetTagValue for array element failed: %v", err)
	}

	// Verify the write
	val, _ = db.GetTagValue(elementName)
	if val != newValue {
		t.Errorf("Element value was not updated. Got %v, want %v", val, newValue)
	}

	// 3. Test out-of-bounds read
	_, err = db.GetTagValue("MyDintArray[5]")
	if err == nil {
		t.Error("Expected an out-of-bounds error for reading, but got nil")
	}

	// 4. Test out-of-bounds write
	err = db.SetTagValue("MyDintArray[5]", plc.DINT(999))
	if err == nil {
		t.Error("Expected an out-of-bounds error for writing, but got nil")
	}

	// 5. Test type mismatch write
	err = db.SetTagValue("MyDintArray[0]", plc.REAL(1.0))
	if err == nil {
		t.Error("Expected a type mismatch error for writing, but got nil")
	}
}

// TestArrayPersistence verifies that array tags can be written to and read from a file.
func TestArrayPersistence(t *testing.T) {
	dbWrite := NewTagDatabase()
	filePath := "array_persistence.txt"
	t.Cleanup(func() { os.Remove(filePath) })

	// Add an array tag
	arrayTag := &Tag{
		Name:  "MyRealArray", // Corrected from TypeARRAY to TypeARRAY
		Value: []plc.REAL{1.1, 2.2, 3.3},
		TypeInfo: &TypeInfo{
			DataType:    TypeARRAY,
			ElementType: TypeREAL,
		},
	}
	dbWrite.AddTag(arrayTag)

	// Write to file
	if err := dbWrite.WriteTagsToFile(filePath); err != nil {
		t.Fatalf("WriteTagsToFile failed: %v", err)
	}

	// Read from file into a new database
	dbRead := NewTagDatabase()
	dbRead.AddTag(&Tag{Name: "MyRealArray", TypeInfo: &TypeInfo{DataType: TypeARRAY, ElementType: TypeREAL}, Value: []plc.REAL{0, 0, 0}})
	if err := dbRead.ReadTagsFromFile(filePath); err != nil {
		t.Fatalf("ReadTagsFromFile failed: %v", err)
	}

	// Verify the value
	val, _ := dbRead.GetTagValue("MyRealArray[1]")
	if val != plc.REAL(2.2) {
		t.Errorf("Array value mismatch after reading from file. Got element [1] = %v, want 2.2", val)
	}
}

// TestMultiDimensionalArrayAccess verifies reading and writing to a multi-dimensional array.
func TestMultiDimensionalArrayAccess(t *testing.T) {
	db := NewTagDatabase()
	tagName := "My2DArray"

	// This represents a 2x3 array: ARRAY[0..1, 0..2] OF DINT
	dims := []int{2, 3}
	// The value is stored in a flat slice of size 2 * 3 = 6
	flatValue := []plc.DINT{
		10, 20, 30, // Row 0
		40, 50, 60, // Row 1
	}

	tag := &Tag{
		Name:  tagName, // Corrected from TypeARRAY to TypeARRAY
		Value: flatValue,
		TypeInfo: &TypeInfo{
			DataType:    TypeARRAY,
			ElementType: TypeDINT,
			Dimensions:  dims,
		},
	}
	db.AddTag(tag)

	// 1. Test reading an element at [1, 1], which is the 5th element (index 4)
	// Flat index = 1*3 + 1 = 4. Value should be 50.
	elementName := "My2DArray[1, 1]"
	val, err := db.GetTagValue(elementName)
	if err != nil {
		t.Fatalf("GetTagValue for multi-dim array element failed: %v", err)
	}
	if val != plc.DINT(50) {
		t.Errorf("Incorrect element value read. Got %v, want %v", val, plc.DINT(50))
	}

	// 2. Test writing to an element at [0, 2], which is the 3rd element (index 2)
	// Flat index = 0*3 + 2 = 2. Value is 30, new value is 35.
	writeElementName := "My2DArray[0, 2]"
	newValue := plc.DINT(35)
	err = db.SetTagValue(writeElementName, newValue)
	if err != nil {
		t.Fatalf("SetTagValue for multi-dim array element failed: %v", err)
	}

	// Verify the write
	val, _ = db.GetTagValue(writeElementName)
	if val != newValue {
		t.Errorf("Element value was not updated. Got %v, want %v", val, newValue)
	}

	// 3. Test out-of-bounds read
	_, err = db.GetTagValue("My2DArray[2, 0]") // First dimension is size 2 (0-1)
	if err == nil {
		t.Error("Expected an out-of-bounds error for reading, but got nil")
	}

	// 4. Test incorrect number of indices
	_, err = db.GetTagValue("My2DArray[1]")
	if err == nil {
		t.Error("Expected an error for incorrect number of indices, but got nil")
	}
}

// TestMultiDimArrayPersistence verifies that multi-dimensional array tags can be persisted.
func TestMultiDimArrayPersistence(t *testing.T) {
	dbWrite := NewTagDatabase()
	filePath := "multi_dim_array_persistence.txt"
	t.Cleanup(func() { os.Remove(filePath) })

	// Define the array in its multi-dimensional form for clarity.
	dims := []int{2, 2}
	multiDimValue := [][]plc.REAL{
		{1.1, 1.2}, // Row 0
		{2.1, 2.2}, // Row 1
	}

	// Flatten the multi-dimensional slice into the 1D representation the database uses.
	flatValue := make([]plc.REAL, 0, dims[0]*dims[1])
	for _, row := range multiDimValue {
		flatValue = append(flatValue, row...)
	}

	tag := &Tag{
		Name:  "Matrix",
		Value: flatValue,
		TypeInfo: &TypeInfo{
			DataType:    TypeARRAY,
			ElementType: TypeREAL,
			Dimensions:  dims,
		}}
	dbWrite.AddTag(tag)

	if err := dbWrite.WriteTagsToFile(filePath); err != nil {
		t.Fatalf("WriteTagsToFile failed: %v", err)
	}

	dbRead := NewTagDatabase()
	// When setting up the read database, it's crucial to define the tag with the correct
	// multi-dimensional TypeInfo, just as an application would on startup.
	dbRead.AddTag(&Tag{
		Name: "Matrix",
		TypeInfo: &TypeInfo{
			DataType:    TypeARRAY,
			ElementType: TypeREAL,
			Dimensions:  dims, // This is the critical fix.
		},
		Value: make([]plc.REAL, 4), // The value will be overwritten by ReadTagsFromFile.
	})
	if err := dbRead.ReadTagsFromFile(filePath); err != nil {
		t.Fatalf("ReadTagsFromFile failed: %v", err)
	}

	// Verify the value of element at [1,0]. The flat index is (1 * 2) + 0 = 2.
	// The value should be 2.1.
	val, err := dbRead.GetTagValue("Matrix[1,0]")
	if err != nil {
		t.Fatalf("GetTagValue for multi-dim array element failed: %v", err)
	}
	if val != plc.REAL(2.1) {
		t.Errorf("Multi-dim array value mismatch after persistence. Got element at [1,0] = %v, want 2.1", val)
	}
}

// TestEnumTag verifies the functionality of ENUM data types.
func TestEnumTag(t *testing.T) {
	// 1. Register a new ENUM type before using it.
	motorStates := []string{"Stopped", "Running", "Faulted"}
	RegisterENUM("MotorState", motorStates)

	db := NewTagDatabase()
	tagName := "MotorStateTag"

	// 2. Add a tag with the new ENUM DataType.
	tag := &Tag{
		Name:  tagName,
		Value: "Stopped", // Initial value must be one of the registered values
		TypeInfo: &TypeInfo{
			DataType: "MotorState", // Use the registered ENUM name as the DataType
		},
	}
	if err := db.AddTag(tag); err != nil {
		t.Fatalf("Failed to add ENUM tag: %v", err)
	}

	// 3. Test setting a valid ENUM value.
	err := db.SetTagValue(tagName, "Running")
	if err != nil {
		t.Fatalf("SetTagValue failed for a valid ENUM value: %v", err) // This test was failing due to a bug in setNestedField
	}

	// Verify the change.
	val, _ := db.GetTagValue(tagName)
	if val != "Running" {
		t.Errorf("ENUM tag value is incorrect. Got '%s', want 'Running'", val)
	}

	// 4. Test setting an invalid ENUM value.
	err = db.SetTagValue(tagName, "Idling") // "Idling" is not in the registered list.
	if err == nil {
		t.Fatal("SetTagValue should have returned an error for an invalid ENUM value.")
	}

	// 5. Test setting a value with the wrong Go type.
	err = db.SetTagValue(tagName, 1) // Should expect a string.
	if err == nil {
		t.Fatal("SetTagValue should have returned an error for a non-string value on an ENUM tag.")
	}
}

// TestSubrangeTag verifies the functionality of subrange validation.
func TestSubrangeTag(t *testing.T) {
	db := NewTagDatabase()
	tagName := "LimitedInt"

	// 1. Add a tag with Min and Max defined.
	tag := &Tag{
		Name:  tagName,
		Value: plc.DINT(50),
		TypeInfo: &TypeInfo{
			DataType: TypeDINT,
			Min:      plc.DINT(0),
			Max:      plc.DINT(100),
		},
	}
	if err := db.AddTag(tag); err != nil {
		t.Fatalf("Failed to add subrange tag: %v", err)
	}

	// 2. Test setting values within the valid range.
	validValues := []plc.DINT{0, 75, 100}
	for _, v := range validValues {
		t.Run(fmt.Sprintf("SetValidValue_%d", v), func(t *testing.T) {
			err := db.SetTagValue(tagName, v)
			if err != nil {
				t.Errorf("SetTagValue failed for valid value %d: %v", v, err)
			}
		})
	}

	// 3. Test setting values outside the valid range.
	invalidValues := []plc.DINT{-1, 101}
	for _, v := range invalidValues {
		t.Run(fmt.Sprintf("SetInvalidValue_%d", v), func(t *testing.T) {
			err := db.SetTagValue(tagName, v)
			if err == nil {
				t.Errorf("SetTagValue should have failed for out-of-range value %d, but it succeeded", v)
			}
		})
	}

	// 4. Test with a REAL subrange.
	realTagName := "LimitedReal" // Corrected from TypeREAL to TypeREAL
	db.AddTag(&Tag{Name: realTagName, TypeInfo: &TypeInfo{DataType: TypeREAL, Min: plc.REAL(0.0), Max: plc.REAL(1.0)}})

	// Should fail
	err := db.SetTagValue(realTagName, plc.REAL(1.1))
	if err == nil {
		t.Error("SetTagValue should have failed for out-of-range REAL value.")
	}

	// 5. Test forcing an out-of-range value.
	_, err = db.SetTagForceValue(tagName, plc.DINT(200))
	if err == nil {
		t.Error("SetTagForceValue should have failed for an out-of-range value.")
	}
}

// TestDirectAddressing verifies that tags can be accessed using IEC direct addresses.
func TestDirectAddressing(t *testing.T) {
	db := NewTagDatabase()

	// 1. Add a BOOL tag that should map to %IX0.0
	boolTagName := "Input_Start"
	boolTag := &Tag{Name: boolTagName, TypeInfo: &TypeInfo{DataType: TypeBOOL}, Value: plc.BOOL(false), DirectAddress: "%IX0.0"}
	db.AddTag(boolTag)

	// 2. Add a REAL tag that should map to %QD0 (assuming REAL is 4 bytes)
	realTagName := "Output_Speed"
	realTag := &Tag{Name: realTagName, TypeInfo: &TypeInfo{DataType: TypeREAL}, Value: plc.REAL(123.45), DirectAddress: "%QD0"}
	db.AddTag(realTag)

	// 3. Add a DINT tag that should map to %MD0 (assuming DINT is 4 bytes)
	dintTagName := "Memory_Counter"
	dintTag := &Tag{Name: dintTagName, TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(500), DirectAddress: "%MD0"}
	db.AddTag(dintTag)

	// Test GetTagValue with direct addresses
	testCases := []struct {
		directAddress string
		expectedValue interface{}
		expectedError bool
	}{
		{"%IX0.0", plc.BOOL(false), false}, // This will get the forced value if set, or the actual value
		{"%QD0", plc.REAL(123.45), false},
		{"%MD0", plc.DINT(500), false},
		{"%IX1.0", nil, true}, // Non-existent direct address
		{"%QX0.0", nil, true}, // Non-existent direct address
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("Get_%s", tc.directAddress), func(t *testing.T) {
			val, err := db.GetTagValue(tc.directAddress)
			if tc.expectedError {
				if err == nil {
					t.Errorf("Expected error for %s, but got none", tc.directAddress)
				}
			} else {
				if err != nil {
					t.Fatalf("Unexpected error for %s: %v", tc.directAddress, err)
				}
				if val != tc.expectedValue {
					t.Errorf("Value mismatch for %s. Got %v, want %v", tc.directAddress, val, tc.expectedValue)
				}
			}
		})
	}

	// Test SetTagValue with direct addresses
	err := db.SetTagValue("%IX0.0", plc.BOOL(true))
	if err != nil {
		t.Fatalf("Failed to set value via direct address %%IX0.0: %v", err)
	}
	// Verify the symbolic tag was updated
	val, _ := db.GetTagValue(boolTagName)
	if val != plc.BOOL(true) {
		t.Errorf("Symbolic tag %s not updated after direct address set. Got %v, want true", boolTagName, val)
	}
}

// TestConstantAndRetainQualifiers verifies the behavior of IsConstant and IsRetain flags.
func TestConstantAndRetainQualifiers(t *testing.T) {
	db := NewTagDatabase()
	constTagName := "MyConstant"
	retainTagName := "MyRetainVar"

	// 1. Add a CONSTANT tag.
	constTag := &Tag{
		Name:  constTagName, // Corrected from TypeDINT to TypeDINT
		Value: plc.DINT(12345),
		TypeInfo: &TypeInfo{
			DataType: TypeDINT},
		Constant: true,
	}

	if err := db.AddTag(constTag); err != nil {
		t.Fatalf("Failed to add constant tag: %v", err)
	}

	// 2. Verify that attempting to set its value fails.
	err := db.SetTagValue(constTagName, plc.DINT(54321))
	if err == nil {
		t.Error("SetTagValue should have failed for a constant tag, but it succeeded.")
	}

	// 3. Verify that attempting to set its force value fails.
	_, err = db.SetTagForceValue(constTagName, plc.DINT(999))
	if err == nil {
		t.Error("SetTagForceValue should have failed for a constant tag, but it succeeded.")
	}

	// 4. Add and verify a RETAIN tag.
	retainTag := &Tag{Name: retainTagName, TypeInfo: &TypeInfo{DataType: TypeREAL}}
	retainTag.Retain = true
	db.AddTag(retainTag)
	tag, found := db.GetTag(retainTagName)
	if !found || !tag.IsRetain() {
		t.Error("Retain flag was not set or retrieved correctly.")
	}
}

// TestStringLengthLimit verifies the functionality of string length limits.
func TestStringLengthLimit(t *testing.T) {
	db := NewTagDatabase()
	tagName := "LimitedString"
	maxLength := 10

	// 1. Add a string tag with a MaxLength.
	tag := &Tag{
		Name:  tagName, // Corrected from TypeSTRING to TypeSTRING
		Value: plc.STRING("initial"),
		TypeInfo: &TypeInfo{
			DataType:  TypeSTRING,
			MaxLength: maxLength,
		},
	}
	if err := db.AddTag(tag); err != nil {
		t.Fatalf("Failed to add string tag with MaxLength: %v", err)
	}

	// 2. Test setting a value within the limit.
	shortString := plc.STRING("short")
	err := db.SetTagValue(tagName, shortString)
	if err != nil {
		t.Fatalf("SetTagValue failed for short string: %v", err) // This test was failing due to a bug in SetValue
	}
	val, _ := db.GetTagValue(tagName)
	if val != shortString {
		t.Errorf("String value mismatch. Got '%s', want '%s'", val, shortString)
	}

	// 3. Test setting a value exceeding the limit (should truncate).
	longString := plc.STRING("this string is too long")
	expectedTruncated := plc.STRING("this strin") // Truncated to 10 characters
	err = db.SetTagValue(tagName, longString)
	if err != nil {
		t.Fatalf("SetTagValue failed for long string: %v", err)
	}
	val, _ = db.GetTagValue(tagName)
	if val != expectedTruncated {
		t.Errorf("String truncation failed. Got '%s', want '%s'", val, expectedTruncated)
	}

	// 4. Test setting a force value exceeding the limit (should truncate).
	forceLongString := plc.STRING("force this string to be too long")
	expectedForceTruncated := plc.STRING("force this")
	_, err = db.SetTagForceValue(tagName, forceLongString)
	if err != nil {
		t.Fatalf("SetTagForceValue failed for long string: %v", err)
	}
	tagAfterForce, _ := db.GetTag(tagName)
	if tagAfterForce.ForceValue != expectedForceTruncated {
		t.Errorf("ForceValue truncation failed. Got '%s', want '%s'", tagAfterForce.ForceValue, expectedForceTruncated)
	}

	// 5. Test persistence of MaxLength.
	filePath := "string_length_persistence.txt"
	t.Cleanup(func() { os.Remove(filePath) })
	db.WriteTagsToFile(filePath)

	dbRead := NewTagDatabase()
	// Add a placeholder with the correct TypeInfo, including MaxLength.
	// This simulates an application restart where tag definitions are known.
	dbRead.AddTag(&Tag{Name: tagName, TypeInfo: &TypeInfo{
		DataType:  TypeSTRING,
		MaxLength: maxLength,
	}, Value: plc.STRING("")})
	dbRead.ReadTagsFromFile(filePath)

	retrievedTag, _ := dbRead.GetTag(tagName)
	if retrievedTag.TypeInfo.MaxLength != maxLength {
		t.Errorf("MaxLength not persisted correctly. Got %d, want %d", retrievedTag.TypeInfo.MaxLength, maxLength)
	}
}

// TestTagSubscription verifies the tag subscription and notification mechanism.
func TestTagSubscription(t *testing.T) {
	db := NewTagDatabase()
	tagName := "SubscribedTag"
	arrayTagName := "SubscribedArray"

	// Add a tag and an array tag
	db.AddTag(&Tag{Name: tagName, TypeInfo: &TypeInfo{DataType: TypeDINT}, Value: plc.DINT(100)})
	db.AddTag(&Tag{Name: arrayTagName, TypeInfo: &TypeInfo{DataType: TypeARRAY, ElementType: TypeDINT}, Value: []plc.DINT{10, 20, 30}})

	var wg sync.WaitGroup
	var receivedTag Tag

	// 1. Subscribe to the simple tag
	wg.Add(1)
	subChannel, subID, err := db.SubscribeToTag(tagName)
	// This goroutine demonstrates a robust subscriber pattern.
	go func() {
		// Use defer to recover from panics within this subscriber,
		// preventing the entire application from crashing.
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Subscriber panicked but should have recovered: %v", r)
			}
		}()

		// Range over the channel to receive updates until it's closed.
		for updatedTag := range subChannel {
			receivedTag = updatedTag
			wg.Done() // Signal that we've received the update for the test.
		}
	}()
	if err != nil {
		t.Fatalf("Failed to subscribe to tag %s: %v", tagName, err)
	}

	// 2. Change the simple tag's value and wait for notification
	newValue := plc.DINT(200)
	err = db.SetTagValue(tagName, newValue)
	if err != nil {
		t.Fatalf("Failed to set tag value: %v", err)
	}
	wg.Wait() // Wait for the subscriber goroutine to receive the update

	if receivedTag.Name != tagName || receivedTag.Value != newValue {
		t.Errorf("Subscriber received incorrect tag or value. Got name: %s, value: %v; Expected name: %s, value: %v",
			receivedTag.Name, receivedTag.Value, tagName, newValue)
	}

	// 3. Unsubscribe and try to change value again (should not notify this subscriber)
	err = db.UnsubscribeFromTag(tagName, subID)
	if err != nil {
		t.Fatalf("Failed to unsubscribe from tag %s: %v", tagName, err)
	}

	// Verify the channel is closed
	select {
	case _, ok := <-subChannel:
		if ok {
			t.Error("Channel should be closed after unsubscribing, but it's still open.")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Timed out waiting for channel to close after unsubscribing.")
	}

	// Change value again - no notification should be received by the unsubscribed channel
	err = db.SetTagValue(tagName, plc.DINT(300))
	if err != nil {
		t.Fatalf("Failed to set tag value after unsubscribe: %v", err)
	}

	// 4. Test subscription for an array element change.
	var receivedArrayTag Tag
	wg.Add(1)
	arraySubChannel, arraySubID, err := db.SubscribeToTag(arrayTagName)
	// Start a goroutine to listen for updates on the array tag.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Array subscriber panicked: %v", r)
			}
		}()
		// For this test, we only expect one update.
		updatedTag, ok := <-arraySubChannel
		if ok {
			receivedArrayTag = updatedTag
			wg.Done()
		}
	}()
	if err != nil {
		t.Fatalf("Failed to subscribe to array tag %s: %v", arrayTagName, err)
	}
	db.SetTagValue(arrayTagName+"[1]", plc.DINT(99)) // Change an element
	wg.Wait()                                        // Wait for the array subscriber goroutine
	currentArrayValue := receivedArrayTag.Value.([]plc.DINT)
	if currentArrayValue[1] != plc.DINT(99) {
		t.Errorf("Array tag subscriber received incorrect value for element 1. Got %v, want %v", currentArrayValue[1], plc.DINT(99))
	}
	db.UnsubscribeFromTag(arrayTagName, arraySubID)
}
