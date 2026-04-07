/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v2.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package honeycomb

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	plc "github.com/apiarytech/royaljelly"
)

// DataType represents the type of a tag.
type DataType string

// Constants for all supported data types, mirroring Types.go
const (
	TypeBOOL     DataType = "BOOL"
	TypeBYTE     DataType = "BYTE"
	TypeWORD     DataType = "WORD"
	TypeDWORD    DataType = "DWORD"
	TypeLWORD    DataType = "LWORD"
	TypeSINT     DataType = "SINT"
	TypeINT      DataType = "INT"
	TypeDINT     DataType = "DINT"
	TypeLINT     DataType = "LINT"
	TypeUSINT    DataType = "USINT"
	TypeUINT     DataType = "UINT"
	TypeUDINT    DataType = "UDINT"
	TypeULINT    DataType = "ULINT"
	TypeREAL     DataType = "REAL"
	TypeLREAL    DataType = "LREAL"
	TypeCOMPLEX  DataType = "COMPLEX"
	TypeLCOMPLEX DataType = "LCOMPLEX"
	TypeSTRING   DataType = "STRING"
	TypeWSTRING  DataType = "WSTRING"
	TypeTIME     DataType = "TIME"
	TypeDATE     DataType = "DATE"
	TypeTOD      DataType = "TOD"
	TypeDT       DataType = "DT"
	TypeENUM     DataType = "ENUM"
	TypeARRAY    DataType = "ARRAY"
)

func init() {
	// Primitive types from the plc library
	typeToDataTypeMap[reflect.TypeOf(plc.INITBOOL)] = TypeBOOL
	typeToDataTypeMap[reflect.TypeOf(plc.INITSINT)] = TypeSINT
	typeToDataTypeMap[reflect.TypeOf(plc.INITINT)] = TypeINT
	typeToDataTypeMap[reflect.TypeOf(plc.INITDINT)] = TypeDINT
	typeToDataTypeMap[reflect.TypeOf(plc.INITLINT)] = TypeLINT
	typeToDataTypeMap[reflect.TypeOf(plc.INITUSINT)] = TypeUSINT
	typeToDataTypeMap[reflect.TypeOf(plc.INITUINT)] = TypeUINT
	typeToDataTypeMap[reflect.TypeOf(plc.INITUDINT)] = TypeUDINT
	typeToDataTypeMap[reflect.TypeOf(plc.INITULINT)] = TypeULINT
	typeToDataTypeMap[reflect.TypeOf(plc.INITREAL)] = TypeREAL
	typeToDataTypeMap[reflect.TypeOf(plc.INITLREAL)] = TypeLREAL
	typeToDataTypeMap[reflect.TypeOf(plc.INITSTRING)] = TypeSTRING
	typeToDataTypeMap[reflect.TypeOf(plc.INITWSTRING)] = TypeWSTRING
	typeToDataTypeMap[reflect.TypeOf(plc.INITTIME)] = TypeTIME
	typeToDataTypeMap[reflect.TypeOf(plc.INITDATE)] = TypeDATE
	typeToDataTypeMap[reflect.TypeOf(plc.INITTOD)] = TypeTOD
	typeToDataTypeMap[reflect.TypeOf(plc.INITDT)] = TypeDT

	// Alias types (BYTE, WORD, etc.)
	typeToDataTypeMap[reflect.TypeOf(plc.INITBYTE)] = TypeBYTE
	typeToDataTypeMap[reflect.TypeOf(plc.INITWORD)] = TypeWORD
	typeToDataTypeMap[reflect.TypeOf(plc.INITDWORD)] = TypeDWORD
	typeToDataTypeMap[reflect.TypeOf(plc.INITLWORD)] = TypeLWORD
}

// TypeInfo holds the defining characteristics of a data type.
// It provides detailed information about the structure and constraints of a tag's value.
type TypeInfo struct {
	DataType    DataType    // DataType specifies the primary data type of the tag (e.g., BOOL, DINT, ARRAY, MotorData).
	ElementType DataType    // ElementType is used when DataType is ARRAY, indicating the type of elements within the array.
	EnumValues  []string    // EnumValues stores the list of valid string values if DataType is ENUM.
	Min         interface{} // Min defines the minimum allowed value for subrange types.
	Max         interface{} // Max defines the maximum allowed value for subrange types.
	MaxLength   int         // MaxLength specifies the maximum length for STRING/WSTRING types. A value of 0 means no limit.
	Dimensions  []int       // Dimensions holds the sizes of each dimension for multi-dimensional arrays (e.g., [2, 3] for a 2x3 array).
}

// Tag represents a single variable (tag) in the system.
// It encapsulates all properties and the current state of a PLC tag.
type Tag struct {
	valMu         sync.RWMutex // valMu provides read/write mutex protection for the tag's internal state.
	Name          string       // Name is the unique symbolic name of the tag.
	Value         interface{}  // Value holds the current data value of the tag.
	Alias         string       // Alias provides an alternative, often shorter, name for the tag.
	DirectAddress string       // DirectAddress stores the IEC 61131-3 direct address (e.g., %IX0.0, %MW10) if applicable.
	TypeInfo      *TypeInfo    // TypeInfo is a pointer to the shared TypeInfo struct defining the tag's data type characteristics.
	Path          string       // Path represents the hierarchical location of the tag within a larger structure or namespace.
	Description   string       // Description provides a human-readable explanation or purpose of the tag.
	Forced        bool         // Forced indicates whether the tag's value is currently being overridden by ForceValue.
	Constant      bool         // Constant, if true, prevents any modification to the tag's Value or ForceValue after creation.
	Retain        bool         // Retain, if true, marks the tag's value for persistence across application restarts.
	ForceValue    interface{}  // ForceValue stores the value that overrides the actual Value when the tag is Forced.
	// Fields for cross-database aliasing
	IsRemoteAlias bool   // If true, this tag is an alias for a tag in another database.
	RemoteDBID    string // The ID of the remote database.
	RemoteTagName string // The name of the tag in the remote database.
}

// UDT (User-Defined Type) defines the interface that any struct-based tag
// must implement to be used within the TagDatabase.
type UDT interface {
	// TypeName returns the unique data type name for this UDT.
	TypeName() DataType
}

var (
	// udtRegistry maps a UDT's string name to its reflect.Type for instantiation.
	udtRegistry = make(map[DataType]reflect.Type)
	// udtMu provides mutex protection for the udtRegistry.
	udtMu sync.RWMutex
)

// RegisterUDT makes a UDT type available to the TagDatabase system.
// This is necessary for creating new instances of the UDT during operations
// like reading from a persistence file.
func RegisterUDT(u UDT) {
	udtMu.Lock()
	defer udtMu.Unlock()

	name := u.TypeName()
	t := reflect.TypeOf(u)

	// Ensure we are registering the struct type itself, not a pointer to it.
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	udtRegistry[name] = t
}

var (
	// enumRegistry maps an ENUM's data type name to its list of valid string values.
	enumRegistry = make(map[DataType][]string)
	// enumMu provides mutex protection for the enumRegistry.
	enumMu sync.RWMutex
)

// RegisterENUM makes an ENUM type available to the TagDatabase system.
func RegisterENUM(name DataType, values []string) {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumRegistry[name] = values
}

// getEnumValues retrieves the possible values for a registered ENUM type.
func getEnumValues(name DataType) ([]string, bool) {
	enumMu.RLock()
	defer enumMu.RUnlock()
	values, ok := enumRegistry[name]
	return values, ok
}

func (t *Tag) GetEnumValues() []string {
	values, _ := getEnumValues(t.TypeInfo.DataType)
	return values
}

// newUDTInstance creates a new instance of a registered UDT by its type name.
func newUDTInstance(name DataType) (UDT, bool) { // Corrected from TypeName() DataType to honeycomb.DataType
	udtMu.RLock()
	defer udtMu.RUnlock()
	t, ok := udtRegistry[name]
	if !ok || t == nil {
		return nil, false
	}

	// Create a new pointer to a struct of the registered type.
	vPtr := reflect.New(t)
	vStruct := vPtr.Elem()

	// Recursively initialize nested UDT fields that are pointers.
	for i := 0; i < vStruct.NumField(); i++ {
		field := vStruct.Field(i)
		if field.Kind() == reflect.Ptr && field.CanSet() {
			// Check if the pointer's element type is a registered UDT.
			udtName, isUDT := getUDTTypeName(field.Type().Elem())
			if isUDT {
				if nestedInstance, ok := newUDTInstance(udtName); ok {
					field.Set(reflect.ValueOf(nestedInstance))
				} else {
					// This case can happen if a nested UDT is used but not registered.
					// We will leave the field as nil, which is reasonable default behavior.
					// A log message could be added here for debugging if desired.
				}
			}
		}
	}

	udt, ok := vPtr.Interface().(UDT)
	return udt, ok
}

// getUDTTypeName checks if a reflect.Type implements the UDT interface and, if so,
// returns its type name without needing to allocate a new instance.
func getUDTTypeName(t reflect.Type) (DataType, bool) {
	// The type must be a struct to be a valid honeycomb.UDT.
	if t.Kind() != reflect.Struct {
		return "", false
	}

	// Check if a pointer to this struct type implements the UDT interface.
	// The TypeName method is typically defined on the pointer receiver.
	udtInterface := reflect.TypeOf((*UDT)(nil)).Elem()
	if reflect.PointerTo(t).Implements(udtInterface) {
		// To get the name, we must create a zero-value instance to call TypeName().
		return reflect.New(t).Interface().(UDT).TypeName(), true
	}
	return "", false
}

// GetName returns the alias of the tag if it is defined, otherwise it returns the base name.
func (t *Tag) GetName() string {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	if t.Alias != "" {
		return t.Alias
	}
	return t.Name
}

// GetValue returns the current value of the tag.
func (t *Tag) GetValue() interface{} {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	if t.Forced {
		// Remote aliases do not have their own force values.
		// The forcing is handled on the remote tag itself.
		if t.IsRemoteAlias {
			// This path should ideally not be hit if GetTagValue is used, but as a safeguard:
			return nil
		}
		return t.ForceValue
	}
	return t.Value
}

// SetValue updates the value of the tag.
// It performs a type check to ensure the new value is compatible with the tag's DataType.
// Note: This method modifies the Tag struct directly. If you retrieved this Tag from a
// TagDatabase, you must use the database's SetTagValue method to ensure the change
// is saved in the thread-safe map.
func (t *Tag) SetValue(value interface{}) error {
	t.valMu.Lock()
	defer t.valMu.Unlock()

	// A Constant tag's value cannot be changed.
	if t.Constant {
		return fmt.Errorf("cannot set value on Constant tag '%s'", t.Name)
	} else if t.TypeInfo.DataType == TypeARRAY { // Corrected from TypeARRAY to honeycomb.TypeARRAY
		val := reflect.ValueOf(value)

		if val.Kind() != reflect.Slice {
			return fmt.Errorf("type mismatch for array tag '%s': expects a slice, but got %T", t.Name, value)
		}
		// Check each element of the slice
		for i := 0; i < val.Len(); i++ {
			elem := val.Index(i).Interface()
			elemType, ok := getDataType(reflect.TypeOf(elem))
			if !ok || elemType != t.TypeInfo.ElementType { // Corrected from TypeInfo.ElementType to honeycomb.TypeInfo.ElementType
				return fmt.Errorf("type mismatch for element %d in array tag '%s': expects DataType %s, but got %T", i, t.Name, t.TypeInfo.ElementType, elem) // Corrected from TypeInfo.ElementType to honeycomb.TypeInfo.ElementType
			}
		}
	} else {
		// If the tag is an ENUM, the incoming value should be a string.
		// The specific ENUM value validation happens later.
		if _, isEnum := getEnumValues(t.TypeInfo.DataType); !isEnum {
			// Not an ENUM, so perform standard primitive and UDT type checking.
			actualDataType, ok := getDataType(reflect.TypeOf(value))
			if !ok {
				return fmt.Errorf("value for tag '%s' has an unsupported type: %T", t.Name, value)
			}
			if actualDataType != t.TypeInfo.DataType {
				return fmt.Errorf("type mismatch for tag '%s': expects DataType %s, but got %s", t.Name, t.TypeInfo.DataType, actualDataType)
			}
		}
	}

	// String length enforcement
	if (t.TypeInfo.DataType == TypeSTRING || t.TypeInfo.DataType == TypeWSTRING) && t.TypeInfo.MaxLength > 0 { // Corrected from TypeSTRING to honeycomb.TypeSTRING
		if strVal, ok := value.(plc.STRING); ok {
			if len(strVal) > t.TypeInfo.MaxLength {
				value = strVal[:t.TypeInfo.MaxLength]
			}
		} else if strVal, ok := value.(string); ok {
			value = strVal[:t.TypeInfo.MaxLength]
		}
	}

	// ENUM Type checking
	if _, isEnum := getEnumValues(t.TypeInfo.DataType); isEnum {
		enumValues := t.GetEnumValues()
		strValue, ok := value.(string)
		if !ok {
			return fmt.Errorf("value for enum tag '%s' has an unsupported type: %T", t.Name, value)
		}
		if !contains(enumValues, strValue) {
			return fmt.Errorf("invalid value '%s' for enum tag '%s'", strValue, t.Name)
		}

	}

	// Subrange validation
	if err := checkSubrange(value, t.TypeInfo.Min, t.TypeInfo.Max); err != nil {
		return fmt.Errorf("value for tag '%s' is out of range: %w", t.Name, err)
	}

	t.Value = value
	return nil
}

// GetForceValue returns the forced value of the tag.
func (t *Tag) GetForceValue() interface{} {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.ForceValue
}

// SetForceValue updates the forced value of the tag.
// It performs a type check to ensure the new value is compatible with the tag's DataType.
func (t *Tag) SetForceValue(value interface{}) error {
	t.valMu.Lock()
	defer t.valMu.Unlock()

	// A Constant tag cannot be forced.
	if t.Constant {
		return fmt.Errorf("cannot set force value on Constant tag '%s'", t.Name)
	}

	// Allow nil to clear the force honeycomb.Value
	if value == nil {
		t.ForceValue = nil
		return nil
	}

	if t.TypeInfo.DataType == TypeARRAY {
		val := reflect.ValueOf(value)
		if val.Kind() != reflect.Slice {
			return fmt.Errorf("type mismatch for array tag '%s': expects a slice for force value, but got %T", t.Name, value)
		}
		// Check each element of the slice
		for i := 0; i < val.Len(); i++ {
			elem := val.Index(i).Interface()
			elemType, ok := getDataType(reflect.TypeOf(elem))
			if !ok || elemType != t.TypeInfo.ElementType {
				return fmt.Errorf("type mismatch for element %d in array force value for tag '%s': expects DataType %s, but got %T", i, t.Name, t.TypeInfo.ElementType, elem)
			}
		}
	} else {
		actualDataType, ok := getDataType(reflect.TypeOf(value))
		if !ok {
			return fmt.Errorf("force value for tag '%s' has an unsupported type: %T", t.Name, value)
		}
		if actualDataType != t.TypeInfo.DataType {
			return fmt.Errorf("type mismatch for tag '%s': expects DataType %s for force value, but got %s", t.Name, t.TypeInfo.DataType, actualDataType)
		}
	}

	// String length enforcement for force value
	if (t.TypeInfo.DataType == TypeSTRING || t.TypeInfo.DataType == TypeWSTRING) && t.TypeInfo.MaxLength > 0 {
		if strVal, ok := value.(plc.STRING); ok {
			if len(strVal) > t.TypeInfo.MaxLength {
				value = strVal[:t.TypeInfo.MaxLength]
			}
		} else if strVal, ok := value.(string); ok {
			if len(strVal) > t.TypeInfo.MaxLength {
				value = strVal[:t.TypeInfo.MaxLength]
			}
		}
	}

	// ENUM Type checking
	if _, isEnum := getEnumValues(t.TypeInfo.DataType); isEnum {
		enumValues := t.GetEnumValues()
		strValue, ok := value.(string)
		if !ok {
			return fmt.Errorf("value for enum tag '%s' has an unsupported type: %T", t.Name, value)
		}
		if !contains(enumValues, strValue) {
			return fmt.Errorf("invalid value '%s' for enum tag '%s'", strValue, t.Name)
		}

	}

	// Subrange validation
	if err := checkSubrange(value, t.TypeInfo.Min, t.TypeInfo.Max); err != nil { // Corrected from TypeInfo.Min to honeycomb.TypeInfo.Min
		return fmt.Errorf("force value for tag '%s' is out of range: %w", t.Name, err)
	}

	t.ForceValue = value
	return nil
}

// GetAlias returns the alias of the tag.
func (t *Tag) GetAlias() string {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.Alias
}

// GetDataType returns the data type of the tag.
func (t *Tag) GetDataType() DataType {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.TypeInfo.DataType
}

// GetDescription returns the description of the tag.
func (t *Tag) GetDescription() string {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.Description
}

// IsForced checks if the tag is currently being forced by examining the ForceMask.
func (t *Tag) IsForced() bool {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.Forced
}

// IsConstant checks if the tag is immutable.
func (t *Tag) IsConstant() bool {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.Constant
}

// IsRetain checks if the tag's value should be persisted across restarts.
func (t *Tag) IsRetain() bool {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.Retain
}

// GetDirectAddress returns the IEC 61131-3 direct address of the tag.
func (t *Tag) GetDirectAddress() string {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.DirectAddress
}

// GetTypeInfo returns a pointer to the TypeInfo struct of the tag.
func (t *Tag) GetTypeInfo() *TypeInfo {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.TypeInfo
}

// GetPath returns the hierarchical path of the tag.
func (t *Tag) GetPath() string {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.Path
}

// Tagger defines the interface for interacting with a single tag.
type Tagger interface {
	GetName() string
	GetAlias() string
	GetDataType() DataType
	GetDescription() string
	IsForced() bool
	GetValue() interface{}
	GetForceValue() interface{}
	GetDirectAddress() string
	GetTypeInfo() *TypeInfo
	IsConstant() bool
	IsRetain() bool
}

// TagDatabaseManager defines the interface for managing a collection of tags.
type TagDatabaseManager interface {
	AddTag(tag *Tag) error
	GetTag(name string) (Tag, bool)
	GetTags(names []string) map[string]Tag
	GetTagsByType(dataType DataType) []Tag
	GetAllTags() []Tag
	GetAllTagNames() []string
	RemoveTag(name string) error
	RenameTag(oldName, newName string) (Tag, error)
	SetTagValue(name string, value interface{}) error
	GetTagValue(name string) (interface{}, error)
	SetTagDescription(name string, description string) error
	GetTagDescription(name string) (string, error)
	SetTagAlias(name string, alias string) error
	GetTagAlias(name string) (string, error)
	SetTagForced(name string, forced bool) (Tag, error)
	GetTagForced(name string) (bool, error)
	SetTagForceValue(name string, value interface{}) (Tag, error)
	GetTagForceValue(name string) (interface{}, error)
	WriteTagsToFile(filePath string) error
	ReadTagsFromFile(filePath string) error
}

// TagDatabase is a thread-safe implementation of the TagDatabaseManager.
type TagDatabase struct {
	tags             sync.Map
	directAddressMap sync.Map                       // map[string]string (direct address -> symbolic name)
	typeRegistry     sync.Map                       // map[DataType]*TypeInfo
	subscriptions    map[string]map[uint64]chan Tag // Changed to chan Tag
	subMu            sync.RWMutex
	dbRegistry       sync.Map // Instance-level registry: map[string]*TagDatabase
}

// DatabaseAccessor defines the interface for any object that can be registered
// as a remote database, allowing for both in-process and networked aliasing.
type DatabaseAccessor interface {
	getTagValueRecursive(name string, depth int) (interface{}, error)
	setTagValueRecursive(name string, value interface{}, depth int) error
}

// NetworkDatabaseClient is an implementation of DatabaseAccessor that communicates
// with a remote TagDatabase server over a network.
// This is a conceptual example; a real implementation would require a network
// protocol (e.g., HTTP, gRPC) and corresponding server-side handlers.

// NewTagDatabase creates and returns a new TagDatabase instance.
func NewTagDatabase() *TagDatabase {
	return &TagDatabase{
		subscriptions: make(map[string]map[uint64]chan Tag),
	}
}

// RegisterDatabase adds a database instance to this instance's local registry.
func (db *TagDatabase) RegisterDatabase(id string, remoteDB DatabaseAccessor) error {
	if _, loaded := db.dbRegistry.LoadOrStore(id, remoteDB); loaded {
		return fmt.Errorf("a database with ID '%s' is already registered with this instance", id)
	}
	return nil
}

// getDatabase retrieves a database instance from this instance's local registry.
func (db *TagDatabase) getDatabase(id string) (DatabaseAccessor, bool) {
	val, found := db.dbRegistry.Load(id)
	if !found {
		return nil, false
	}
	return val.(DatabaseAccessor), true
}

// SubscribeToTag allows a client to register a callback function to be notified
// when the value of a specific tag changes. It returns a unique subscription ID
// and an error if the tag does not exist.
// The callback function receives a copy of the updated Tag struct.
//
// The client should store the returned subscription ID to later unsubscribe.
// directAddressRegex matches IEC direct addresses like %IX1.0, %QW10, %MD20
var directAddressRegex = regexp.MustCompile(`^%([IQM])([XBWDL])(\d+)(?:\.(\d+))?$`)

func (db *TagDatabase) SubscribeToTag(tagName string) (<-chan Tag, uint64, error) {
	if _, found := db.tags.Load(tagName); !found {
		return nil, 0, fmt.Errorf("tag '%s' not found for subscription", tagName)
	}

	db.subMu.Lock()
	defer db.subMu.Unlock()

	if _, ok := db.subscriptions[tagName]; !ok {
		db.subscriptions[tagName] = make(map[uint64]chan Tag)
	} // Corrected from TypeARRAY to honeycomb.TypeARRAY

	ch := make(chan Tag, 1) // Buffered channel to avoid blocking the publisher
	// Use a random number for the ID to avoid overflow and make it unpredictable.
	randVal, _ := plc.RAND(plc.ULINT(0))
	id := reflect.ValueOf(randVal).Uint()
	// Ensure the ID is unique for this tag's subscriptions.
	for _, exists := db.subscriptions[tagName][id]; exists; _, exists = db.subscriptions[tagName][id] {
		randVal, _ = plc.RAND(plc.ULINT(0))
		id = reflect.ValueOf(randVal).Uint()
	}
	db.subscriptions[tagName][id] = ch
	return ch, id, nil
}

// UnsubscribeFromTag removes a registered callback function using its subscription ID.
// It returns an error if the tag or subscription ID does not exist.
func (db *TagDatabase) UnsubscribeFromTag(tagName string, subscriptionID uint64) error {
	db.subMu.Lock()
	defer db.subMu.Unlock()

	if subs, ok := db.subscriptions[tagName]; ok {
		if ch, found := subs[subscriptionID]; found {
			delete(subs, subscriptionID)
			close(ch) // Close the channel to signal to consumers that no more data will be sent.
			if len(subs) == 0 {
				delete(db.subscriptions, tagName) // Clean up if no more subscribers for this tag
			}
			return nil
		}
	}
	return fmt.Errorf("subscription ID %d not found for tag '%s'", subscriptionID, tagName)
}

// AddTag adds a new tag to the database. It returns an error if a tag with the same name already exists.
func (db *TagDatabase) AddTag(tag *Tag) error {
	// For non-alias tags, if TypeInfo is not already provided, resolve and assign it.
	// If it is provided (common when pre-adding tags for persistence), we still need to
	// ensure it's registered in the type registry.
	if !tag.IsRemoteAlias && tag.TypeInfo == nil {
		typeInfo, err := db.getOrRegisterTypeInfo(tag)
		if err != nil {
			return fmt.Errorf("error processing type for tag '%s': %w", tag.Name, err)
		}
		tag.TypeInfo = typeInfo // Assign the inferred or registered TypeInfo
	}

	// LoadOrStore is an atomic operation that checks for existence and stores if not present. // Corrected from TypeInfo to honeycomb.TypeInfo
	// It returns the existing value if the key was already there.
	if _, loaded := db.tags.LoadOrStore(tag.Name, tag); loaded {
		return fmt.Errorf("tag '%s' already exists in the database", tag.Name)
	}

	// If the tag is a UDT and its value is nil, create a new zero-value instance.
	// This ensures that UDT tags always have a valid, non-nil value.
	// We need to load the tag we just stored to modify it.
	newTag, _ := db.tags.Load(tag.Name) // Corrected from TypeInfo to honeycomb.TypeInfo
	tagPtr := newTag.(*Tag)

	tagPtr.valMu.Lock()
	// Only attempt to auto-instantiate UDTs for non-alias tags with valid TypeInfo.
	if !tagPtr.IsRemoteAlias && tagPtr.TypeInfo != nil {
		_, isUDT := newUDTInstance(tagPtr.TypeInfo.DataType)
		if isUDT && tagPtr.Value == nil {
			if instance, ok := newUDTInstance(tagPtr.TypeInfo.DataType); ok {
				tagPtr.Value = instance
			}
		}
	}
	tagPtr.valMu.Unlock()

	// If the tag is a PLC memory address, generate and store its direct address mapping.
	if directAddr, ok := generateDirectAddress(tag); ok {
		tag.DirectAddress = directAddr
	}

	if tag.DirectAddress != "" {
		db.directAddressMap.Store(tag.DirectAddress, tag.Name)
	}
	return nil
}

// getOrRegisterTypeInfo finds or creates a TypeInfo struct for a given tag.
func (db *TagDatabase) getOrRegisterTypeInfo(tag *Tag) (*TypeInfo, error) {
	// If TypeInfo is already provided, generate a key and check the registry.
	if tag.TypeInfo != nil { // Corrected from TypeInfo to honeycomb.TypeInfo
		key := generateTypeInfoKey(tag.TypeInfo)
		if existing, found := db.typeRegistry.Load(key); found {
			return existing.(*TypeInfo), nil
		}
		// If not found, register the provided TypeInfo.
		db.typeRegistry.Store(key, tag.TypeInfo)
		return tag.TypeInfo, nil
	}

	// If TypeInfo is nil, we must construct it from the tag's properties. // Corrected from TypeInfo to honeycomb.TypeInfo
	newTypeInfo := &TypeInfo{}
	// If the value is nil, we cannot infer the type.
	if tag.Value == nil {
		return nil, fmt.Errorf("cannot infer type for tag '%s' because its Value is nil and TypeInfo was not provided", tag.Name)
	}

	dataType, ok := getDataType(reflect.TypeOf(tag.Value))
	if !ok {
		return nil, fmt.Errorf("could not determine data type from tag value of type %T", tag.Value)
	}
	newTypeInfo.DataType = dataType

	if dataType == TypeARRAY {
		sliceType := reflect.TypeOf(tag.Value) // Corrected from TypeARRAY to honeycomb.TypeARRAY
		elemType, elemOk := getDataType(sliceType.Elem())
		if !elemOk {
			return nil, fmt.Errorf("could not determine element type for array tag '%s'", tag.Name)
		}
		newTypeInfo.ElementType = elemType
	}

	// Generate a unique key for this new type definition.
	key := generateTypeInfoKey(newTypeInfo)
	if existing, found := db.typeRegistry.Load(key); found {
		return existing.(*TypeInfo), nil
	}

	// Atomically add the new TypeInfo to the registry.
	actual, _ := db.typeRegistry.LoadOrStore(key, newTypeInfo)
	return actual.(*TypeInfo), nil
}

// generateTypeInfoKey creates a unique string key for a given TypeInfo.
func generateTypeInfoKey(ti *TypeInfo) string {
	var keyBuilder strings.Builder
	keyBuilder.WriteString(string(ti.DataType))

	if ti.DataType == TypeARRAY { // Corrected from TypeARRAY to honeycomb.TypeARRAY
		keyBuilder.WriteString(fmt.Sprintf("[%s]", ti.ElementType))
	}
	if ti.MaxLength > 0 {
		keyBuilder.WriteString(fmt.Sprintf("(%d)", ti.MaxLength))
	}
	if ti.Min != nil || ti.Max != nil {
		minStr := "nil"
		maxStr := "nil"
		if ti.Min != nil {
			minStr = fmt.Sprintf("%v", ti.Min)
		}
		if ti.Max != nil {
			maxStr = fmt.Sprintf("%v", ti.Max)
		}
		keyBuilder.WriteString(fmt.Sprintf("_subrange_%s_%s", minStr, maxStr))
	}

	return keyBuilder.String()
}

// GetTag retrieves a tag by its name. It returns the tag and true if found, otherwise an empty Tag and false.
func (db *TagDatabase) GetTag(name string) (Tag, bool) {
	// First, try to find a direct match for the full name.
	val, found := db.tags.Load(name)
	if found {
		tagPtr := val.(*Tag)
		tagPtr.valMu.RLock() // Corrected from TypeARRAY to honeycomb.TypeARRAY
		defer tagPtr.valMu.RUnlock()
		return Tag{
			Name:        tagPtr.Name,
			Value:       tagPtr.Value,
			Alias:       tagPtr.Alias,
			TypeInfo:    tagPtr.TypeInfo,
			Description: tagPtr.Description,
			Forced:      tagPtr.Forced,
			Constant:    tagPtr.Constant,
			Retain:      tagPtr.Retain,
			ForceValue:  tagPtr.ForceValue,
		}, true
	}

	// If not found, check for nested UDT field access (e.g., "MyUDT.Field").
	if strings.Contains(name, ".") {
		// This path is for read-only access to a nested field as if it were a tag.
		// It returns a temporary honeycomb.Tag struct representing the field.
		nestedtag, err := db.getNestedField(name)
		if err != nil {
			return Tag{}, false // The nested field was not found, so return false.
		}
		return Tag{
			Name:        nestedtag.Name,
			Value:       nestedtag.Value, // This is the field's value
			Alias:       nestedtag.Alias,
			TypeInfo:    nestedtag.TypeInfo,
			Description: nestedtag.Description, // This could be the field's description if we add it
			Forced:      nestedtag.Forced,
			ForceValue:  nestedtag.ForceValue,
		}, true
	}

	// Check for array element access (e.g., "MyArray[2]").
	if strings.Contains(name, "[") && strings.HasSuffix(name, "]") {
		element, err := db.GetTagValue(name)
		if err != nil { // Corrected from TypeARRAY to honeycomb.TypeARRAY
			return Tag{}, false
		}
		// Return a temporary Tag representing the element.
		elemDataType, _ := getDataType(reflect.TypeOf(element))
		return Tag{
			Name:  name,
			Value: element,
			TypeInfo: &TypeInfo{
				DataType:    elemDataType,
				ElementType: elemDataType, // For a single element, ElementType is the same
			},
		}, true
	}

	return Tag{}, false
}

// GetAllTags returns a slice of all tags currently in the database.
func (db *TagDatabase) GetAllTags() []Tag {
	var tags []Tag
	db.tags.Range(func(key, value interface{}) bool {
		tagPtr := value.(*Tag)
		tagPtr.valMu.RLock()
		tags = append(tags, Tag{
			Name:        tagPtr.Name,
			Value:       tagPtr.Value,
			Alias:       tagPtr.Alias,
			TypeInfo:    tagPtr.TypeInfo,
			Description: tagPtr.Description,
			Forced:      tagPtr.Forced,
			Constant:    tagPtr.Constant,
			Retain:      tagPtr.Retain,
			ForceValue:  tagPtr.ForceValue,
		})
		tagPtr.valMu.RUnlock()
		return true
	})
	return tags
}

// GetTags retrieves multiple tags by their names in a single, thread-safe operation.
// It returns a map of tag names to the found Tag structs.
// Tags that are not found in the database will be omitted from the result map.
func (db *TagDatabase) GetTags(names []string) map[string]Tag {
	foundTags := make(map[string]Tag)
	for _, name := range names {
		if val, found := db.tags.Load(name); found {
			tagPtr := val.(*Tag)
			tagPtr.valMu.RLock()
			foundTags[name] = Tag{
				Name:        tagPtr.Name,
				Value:       tagPtr.Value,
				Alias:       tagPtr.Alias,
				TypeInfo:    tagPtr.TypeInfo,
				Description: tagPtr.Description,
				Constant:    tagPtr.Constant,
				Retain:      tagPtr.Retain,
				Forced:      tagPtr.Forced,
				ForceValue:  tagPtr.ForceValue,
			}
			tagPtr.valMu.RUnlock()
		}
	}
	return foundTags
}

// GetTagsByType returns a slice of all tags that match the given DataType.
func (db *TagDatabase) GetTagsByType(dataType DataType) []Tag {
	matchingTags := make([]Tag, 0)
	db.tags.Range(func(key, value interface{}) bool {
		tag := value.(*Tag) // No need to lock for read-only properties
		if tag.TypeInfo.DataType == dataType {
			tag.valMu.RLock()
			matchingTags = append(matchingTags, Tag{
				Name:        tag.Name,
				Value:       tag.Value,
				Alias:       tag.Alias,
				TypeInfo:    tag.TypeInfo,
				Description: tag.Description,
				Constant:    tag.Constant,
				Retain:      tag.Retain,
				Forced:      tag.Forced,
				ForceValue:  tag.ForceValue,
			})
			tag.valMu.RUnlock()
		}
		return true
	})
	return matchingTags
}

// GetAllTagNames returns a slice of all tag names currently in the database.
func (db *TagDatabase) GetAllTagNames() []string {
	var names []string
	db.tags.Range(func(key, value interface{}) bool {
		names = append(names, key.(string))
		return true
	})
	return names
}

// RemoveTag deletes a tag from the database by its name.
// It returns an error if the tag does not exist.
func (db *TagDatabase) RemoveTag(name string) error {
	val, loaded := db.tags.LoadAndDelete(name)
	if !loaded {
		return fmt.Errorf("tag '%s' not found in database", name)
	}

	// Also remove any active subscriptions for this tag.
	db.subMu.Lock()
	if subs, found := db.subscriptions[name]; found {
		// Close all channels to notify subscribers that the tag is gone.
		for _, ch := range subs {
			close(ch)
		}
		// Remove the entry from the subscriptions map.
		delete(db.subscriptions, name)
	}
	db.subMu.Unlock()

	// If the removed tag had a direct address, we must also remove it from the directAddressMap.
	if tag, ok := val.(*Tag); ok {
		// If the tag itself has a direct address, remove it.
		if tag.DirectAddress != "" {
			db.directAddressMap.Delete(tag.DirectAddress)
		}

		// If the tag is an array, we must also remove the direct address mappings for all its elements.
		// This is common for tags created by PopulateDatabaseFromVariables.
		if tag.TypeInfo != nil && tag.TypeInfo.DataType == TypeARRAY {
			if sliceVal := reflect.ValueOf(tag.Value); sliceVal.Kind() == reflect.Slice {
				re := regexp.MustCompile(`^([IQM])\.([BWDLR])$`)
				matches := re.FindStringSubmatch(tag.Name)
				if len(matches) == 3 {
					for i := 0; i < sliceVal.Len(); i++ {
						if directAddr, ok := generateDirectAddressForElement(matches[1], matches[2], tag.TypeInfo.ElementType, i); ok {
							db.directAddressMap.Delete(directAddr)
						}
					}
				}
			}
		}
	}
	return nil
}

// RenameTag changes the name of an existing tag from oldName to newName.
// This operation is atomic and thread-safe. It will fail if the newName
// already exists or if the oldName cannot be found.
func (db *TagDatabase) RenameTag(oldName, newName string) (Tag, error) {
	// 1. Atomically load and delete the old tag.
	val, found := db.tags.LoadAndDelete(oldName)
	if !found {
		return Tag{}, fmt.Errorf("tag '%s' not found in database", oldName)
	}
	tagPtr := val.(*Tag)

	// 2. Atomically "claim" the new name. LoadOrStore will store the tagPtr
	// only if newName is not already in the map.
	actual, loaded := db.tags.LoadOrStore(newName, tagPtr)
	if loaded {
		// The newName was already taken between our check and our store.
		// Roll back: put the old tag back where it was.
		if actual != tagPtr { // Check if it was taken by a different tag.
			db.tags.Store(oldName, tagPtr) // Rollback
			return Tag{}, fmt.Errorf("cannot rename to '%s', a tag with that name already exists", newName)
		}
		// If actual == tagPtr, it's a no-op rename (e.g. "A" to "A"). We can proceed.
	}

	// 3. Now that the new name is secured, update the internal name field.
	tagPtr.valMu.Lock()
	defer tagPtr.valMu.Unlock()

	tagPtr.Name = newName
	db.tags.Store(newName, tagPtr)

	// Also migrate any active subscriptions from the old name to the new name.
	db.subMu.Lock()
	if subs, found := db.subscriptions[oldName]; found {
		// If there are no existing subscriptions for the new name (which should be the case),
		// simply move the map of subscriptions.
		if _, exists := db.subscriptions[newName]; !exists {
			db.subscriptions[newName] = subs
			delete(db.subscriptions, oldName)
		}
	}
	db.subMu.Unlock()

	// Update the directAddressMap for the new name.
	if tagPtr.DirectAddress != "" {
		// For a simple tag, just update the single mapping.
		db.directAddressMap.Delete(tagPtr.DirectAddress)
		db.directAddressMap.Store(tagPtr.DirectAddress, newName)
	} else if tagPtr.TypeInfo != nil && tagPtr.TypeInfo.DataType == TypeARRAY {
		// For an array tag, we need to update the mapping for each element.
		if sliceVal := reflect.ValueOf(tagPtr.Value); sliceVal.Kind() == reflect.Slice {
			re := regexp.MustCompile(`^([IQM])\.([BWDLR])$`)
			// We check against the newName's potential prefix, but use oldName to find matches
			// as the tag's internal name was just changed.
			matches := re.FindStringSubmatch(oldName)
			if len(matches) == 3 {
				for i := 0; i < sliceVal.Len(); i++ {
					if directAddr, ok := generateDirectAddressForElement(matches[1], matches[2], tagPtr.TypeInfo.ElementType, i); ok {
						// Delete the old mapping (e.g., %IX0.0 -> I.B[0])
						db.directAddressMap.Delete(directAddr)
						// Add the new mapping (e.g., %IX0.0 -> MyInputs[0])
						newElementName := fmt.Sprintf("%s[%d]", newName, i)
						db.directAddressMap.Store(directAddr, newElementName)
					}
				}
			}
		}
	}

	// Create and return a safe copy of the tag's state.
	return Tag{
		Name:        tagPtr.Name,
		Value:       tagPtr.Value,
		Alias:       tagPtr.Alias,
		TypeInfo:    tagPtr.TypeInfo,
		Description: tagPtr.Description,
		Constant:    tagPtr.Constant,
		Retain:      tagPtr.Retain,
		Forced:      tagPtr.Forced,
		ForceValue:  tagPtr.ForceValue,
	}, nil
}

// SetTagValue updates the value of an existing tag in the database.
// It performs a type check to ensure the new value is compatible with the tag's DataType.
func (db *TagDatabase) SetTagValue(name string, value interface{}) error {
	// If the value being set is itself a UDT, we should treat it as a wholesale
	return db.setTagValueRecursive(name, value, 0)
}

func (db *TagDatabase) setTagValueRecursive(name string, value interface{}, depth int) (err error) {
	// First, check if the name is a direct address.
	if directAddressRegex.MatchString(name) {
		if symbolicName, found := db.directAddressMap.Load(name); found {
			name = symbolicName.(string) // Use the resolved symbolic name
		} else {
			return fmt.Errorf("SetTagValue: direct address '%s' not found in database", name)
		}
	}

	// Check for remote alias before any other processing.
	if val, found := db.tags.Load(name); found {
		tag := val.(*Tag)
		if tag.IsRemoteAlias {
			if depth > 10 { // Prevent infinite recursion
				return fmt.Errorf("max recursion depth exceeded for remote alias '%s'", name)
			}
			remoteDB, found := db.getDatabase(tag.RemoteDBID)
			if !found {
				return fmt.Errorf("remote database with ID '%s' not found for alias '%s'", tag.RemoteDBID, name)
			}
			// Call the remote database's SetTagValue.
			return remoteDB.setTagValueRecursive(tag.RemoteTagName, value, depth+1)
		}
	}

	// If the value being set is itself a UDT, we should treat it as a wholesale
	// replacement of the tag's value, not a nested field write, even if the name // Corrected from TypeARRAY to honeycomb.TypeARRAY
	// contains dots (which it shouldn't for this case, but we check defensively).
	if _, isUDT := value.(UDT); isUDT {
		return db.setSimpleTagValue(name, value)
	}

	// Handle compound access like "MyArray[1].MyField"
	if strings.Contains(name, "[") && strings.Contains(name, ".") {
		// Find the last ']' to correctly parse paths like "MyArray[1].NestedStruct.Field"
		lastBracket := strings.LastIndex(name, "]")
		if lastBracket != -1 && lastBracket < len(name)-1 {
			arrayPart := name[:lastBracket+1]
			fieldPart := name[lastBracket+2:] // +2 to skip the '.'

			// This is a recursive call to handle the nested field part
			// on the result of the array access part.
			return db.setNestedField(arrayPart, value, fieldPart)
		}
	}

	// Check for array element access.
	if strings.Contains(name, "[") && strings.HasSuffix(name, "]") {
		baseTag, index, err := db.parseArrayAccess(name)
		if err != nil {
			return err
		}
		// Lock, type check, and set the value.
		if err := setArrayElementValue(baseTag, index, value); err != nil {
			return err
		}
		db.notifySubscribers(baseTag) // Notify subscribers of the base array tag
		return nil
	}

	// Otherwise, check for nested UDT field access.
	if strings.Contains(name, ".") {
		parts := strings.SplitN(name, ".", 2)
		basePath := parts[0]
		fieldPath := parts[1]
		return db.setNestedField(basePath, value, fieldPath)
	}

	// If not nested, proceed with updating the whole tag value.
	return db.setSimpleTagValue(name, value)
}

// GetTagValue retrieves the value of a tag by its name.
func (db *TagDatabase) GetTagValue(name string) (interface{}, error) {
	return db.getTagValueRecursive(name, 0)
}

// getTagValueRecursive is the core implementation for retrieving a tag's value.
// It handles various access patterns in a specific order:
// 1. Direct Address Resolution (e.g., "%IX0.0")
// 2. Direct Tag Match (e.g., "MyTag")
// 3. Remote Alias Resolution (if a direct match is a remote alias)
// 4. Compound Access (e.g., "MyArray[0].MyField")
// 5. Array Element Access (e.g., "MyArray[0]")
// 6. Nested UDT Field Access (e.g., "MyUDT.MyField")
// The `depth` parameter is used to prevent infinite recursion in chained remote aliases.
func (db *TagDatabase) getTagValueRecursive(name string, depth int) (interface{}, error) {
	// STEP 1: Direct Address Resolution.
	// Check if the name matches the pattern for an IEC direct address (e.g., %IX0.0, %MW100).
	if directAddressRegex.MatchString(name) {
		// If it's a direct address, look up its corresponding symbolic name in the map.
		if symbolicName, found := db.directAddressMap.Load(name); found {
			name = symbolicName.(string) // Replace the address with the symbolic name for further processing.
		} else {
			return nil, fmt.Errorf("GetTagValue: direct address '%s' not found in database", name)
		}
	}

	// STEP 2: Direct Tag Match.
	// Check if the name (which could now be a resolved symbolic name) exists as a top-level tag.
	val, found := db.tags.Load(name)
	if found {
		tag := val.(*Tag)
		// STEP 3: Remote Alias Resolution.
		// If the found tag is a remote alias, we must delegate the request to the target database.
		if tag.IsRemoteAlias {
			if depth > 10 { // Safety check to prevent infinite loops in alias chains.
				return nil, fmt.Errorf("max recursion depth exceeded for remote alias '%s'", name)
			}
			remoteDB, found := db.getDatabase(tag.RemoteDBID)
			if !found {
				return nil, fmt.Errorf("remote database with ID '%s' not found for alias '%s'", tag.RemoteDBID, name)
			}
			// Recursively call this function on the remote DB with the remote tag name.
			return remoteDB.getTagValueRecursive(tag.RemoteTagName, depth+1)
		}
		// If it's a regular tag, return its value, respecting the forced status.
		return tag.GetValue(), nil // Use GetValue() to respect forcing
	}

	// If no direct tag was found, we check for more complex access patterns.
	// STEP 4: Compound Access (e.g., "MyArray[0].MyField").
	// This pattern involves both array access and nested field access.
	if strings.Contains(name, "[") && strings.Contains(name, ".") {
		lastBracket := strings.LastIndex(name, "]")
		if lastBracket != -1 && lastBracket < len(name)-1 {
			arrayPart := name[:lastBracket+1] // e.g., "MyArray[0]"
			fieldPart := name[lastBracket+2:] // e.g., "MyField" (+2 to skip the ']').

			// First, recursively resolve the array element part to get the UDT instance.
			element, err := db.getTagValueRecursive(arrayPart, depth) // Pass depth
			if err != nil {
				return nil, err
			}
			// Then, get the specific field from that UDT instance.
			return getFieldFromStruct(element, fieldPart)
		}
	}

	// STEP 5: Array Element Access (e.g., "MyArray[0]" or "My2DArray[1,2]").
	if strings.Contains(name, "[") && strings.HasSuffix(name, "]") {
		// Parse the name to get the base array tag and the calculated flat index.
		baseTag, index, err := db.parseArrayAccess(name)
		if err != nil {
			return nil, err
		}
		// Retrieve the element value from the array.
		return getArrayElementValue(baseTag, index)
	}

	// STEP 6: Nested UDT Field Access (e.g., "MyUDT.MyField").
	if strings.Contains(name, ".") {
		// getNestedField handles parsing the path and traversing the struct.
		nestedTag, err := db.getNestedField(name)
		if err != nil {
			return nil, fmt.Errorf("GetTagValue: %w", err)
		}
		return nestedTag.Value, nil
	}

	// If none of the above patterns match, the tag does not exist.
	return nil, fmt.Errorf("GetTagValue: tag '%s' not found in database", name)
}

// SetTagDescription updates the Description for a given tag.
func (db *TagDatabase) SetTagDescription(name string, description string) (Tag, error) {
	val, found := db.tags.Load(name)
	if !found {
		return Tag{}, fmt.Errorf("SetTagDescription: tag '%s' not found in database", name)
	}

	tagPtr := val.(*Tag)
	tagPtr.valMu.Lock() // Corrected from TypeARRAY to honeycomb.TypeARRAY
	tagPtr.Description = description
	tagPtr.valMu.Unlock()
	// Create and return a safe copy of the tag's state.
	return Tag{
		Name:        tagPtr.Name,
		Value:       tagPtr.Value,
		Alias:       tagPtr.Alias,
		TypeInfo:    tagPtr.TypeInfo,
		Description: tagPtr.Description,
		Constant:    tagPtr.Constant,
		Retain:      tagPtr.Retain,
		Forced:      tagPtr.Forced,
		ForceValue:  tagPtr.ForceValue,
	}, nil
}

// SetTagAlias updates the Alias for a given tag.
func (db *TagDatabase) SetTagAlias(name string, alias string) error {
	val, found := db.tags.Load(name)
	if !found {
		return fmt.Errorf("SetTagAlias: tag '%s' not found in database", name)
	}

	tagPtr := val.(*Tag)
	tagPtr.valMu.Lock() // Corrected from TypeARRAY to honeycomb.TypeARRAY
	tagPtr.Alias = alias
	tagPtr.valMu.Unlock()
	return nil
}

// GetTagAlias retrieves the Alias of a tag by its name.
func (db *TagDatabase) GetTagAlias(name string) (string, error) {
	val, found := db.tags.Load(name)
	if !found {
		return "", fmt.Errorf("GetTagAlias: tag '%s' not found in database", name)
	}
	tag := val.(*Tag)
	return tag.Alias, nil
}

// SetTagForced updates the Forced flag for a given tag.
func (db *TagDatabase) SetTagForced(name string, forced bool) (Tag, error) {
	val, found := db.tags.Load(name)
	if !found {
		return Tag{}, fmt.Errorf("SetTagForced: tag '%s' not found in database", name)
	}
	tag := val.(*Tag)
	tag.valMu.Lock() // Corrected from TypeARRAY to honeycomb.TypeARRAY
	tag.Forced = forced
	tag.valMu.Unlock()
	// Create and return a safe copy of the tag's state.
	return Tag{
		Name:        tag.Name,
		Value:       tag.Value,
		Alias:       tag.Alias,
		TypeInfo:    tag.TypeInfo,
		Description: tag.Description,
		Forced:      tag.Forced,
		Constant:    tag.Constant,
		Retain:      tag.Retain,
		ForceValue:  tag.ForceValue,
	}, nil
}

// GetTagDescription retrieves the Description of a tag by its name.
func (db *TagDatabase) GetTagDescription(name string) (string, error) {
	val, found := db.tags.Load(name)
	if !found {
		return "", fmt.Errorf("GetTagDescription: tag '%s' not found in database", name)
	}
	tag := val.(*Tag)
	return tag.Description, nil
}

// GetTagForced retrieves the Forced status of a tag by its name.
func (db *TagDatabase) GetTagForced(name string) (bool, error) {
	val, found := db.tags.Load(name)
	if !found {
		return false, fmt.Errorf("GetTagForced: tag '%s' not found in database", name)
	}
	tag := val.(*Tag)
	tag.valMu.RLock()
	defer tag.valMu.RUnlock()
	return tag.Forced, nil
}

// SetTagForceValue updates the ForceValue for a given tag.
// It performs a type check to ensure the new value is compatible with the tag's DataType.
func (db *TagDatabase) SetTagForceValue(name string, value interface{}) (Tag, error) {
	val, found := db.tags.Load(name)
	if !found {
		return Tag{}, fmt.Errorf("SetTagForceValue: tag '%s' not found in database", name)
	}
	tag := val.(*Tag)
	// A Constant tag cannot have its force value set.
	if tag.Constant {
		return Tag{}, fmt.Errorf("cannot set force value on Constant tag '%s'", tag.Name)
	}

	// Lock the tag to safely perform the type check and update.
	tag.valMu.Lock()
	defer tag.valMu.Unlock()

	// Allow nil to clear the force honeycomb.Value
	if value == nil {
		tag.ForceValue = nil
	} else {
		if tag.TypeInfo.DataType == TypeARRAY {
			val := reflect.ValueOf(value)
			if val.Kind() != reflect.Slice {
				return Tag{}, fmt.Errorf("type mismatch for array tag '%s': expects a slice for force value, but got %T", tag.Name, value)
			}
			// Check each element of the slice
			for i := 0; i < val.Len(); i++ {
				elem := val.Index(i).Interface()
				elemType, ok := getDataType(reflect.TypeOf(elem))
				if !ok || elemType != tag.TypeInfo.ElementType {
					return Tag{}, fmt.Errorf("type mismatch for element %d in array force value for tag '%s': expects DataType %s, but got %T", i, tag.Name, tag.TypeInfo.ElementType, elem)
				}
			}
			tag.ForceValue = value
		} else {
			actualDataType, ok := getDataType(reflect.TypeOf(value))
			if !ok {
				return Tag{}, fmt.Errorf("force value for tag '%s' has an unsupported type: %T", tag.Name, value)
			}
			if actualDataType != tag.TypeInfo.DataType {
				return Tag{}, fmt.Errorf("type mismatch for tag '%s': expects DataType %s for force value, but got %s", tag.Name, tag.TypeInfo.DataType, actualDataType)
			}
			// Subrange validation for the force value.
			if err := checkSubrange(value, tag.TypeInfo.Min, tag.TypeInfo.Max); err != nil {
				return Tag{}, fmt.Errorf("force value for tag '%s' is out of range: %w", tag.Name, err)
			}

			// String length enforcement for force value.
			if (tag.TypeInfo.DataType == TypeSTRING || tag.TypeInfo.DataType == TypeWSTRING) && tag.TypeInfo.MaxLength > 0 {
				if strVal, ok := value.(plc.STRING); ok {
					if len(strVal) > tag.TypeInfo.MaxLength {
						value = strVal[:tag.TypeInfo.MaxLength]
					}
				} else if strVal, ok := value.(string); ok {
					if len(strVal) > tag.TypeInfo.MaxLength {
						value = strVal[:tag.TypeInfo.MaxLength]
					}
				}
			}
			tag.ForceValue = value
		}
	}
	// Create and return a safe copy of the tag's state.
	return Tag{
		Name:        tag.Name,
		Value:       tag.Value,
		Alias:       tag.Alias,
		TypeInfo:    tag.TypeInfo,
		Description: tag.Description,
		Forced:      tag.Forced,
		Constant:    tag.Constant,
		Retain:      tag.Retain,
		ForceValue:  tag.ForceValue,
	}, nil
}

// GetTagForceValue retrieves the ForceValue of a tag by its name.
func (db *TagDatabase) GetTagForceValue(name string) (interface{}, error) {
	val, found := db.tags.Load(name)
	if !found {
		return nil, fmt.Errorf("GetTagForceValue: tag '%s' not found in database", name)
	}
	tag := val.(*Tag)
	tag.valMu.RLock()
	defer tag.valMu.RUnlock()
	return tag.ForceValue, nil
}

// notifySubscribers iterates through all subscriptions for a given tag and invokes their callbacks.
// It passes a copy of the tag's data to avoid external modification of the internal state.
func (db *TagDatabase) notifySubscribers(tag *Tag) {
	// First, create a safe, clean copy of the tag's data. This requires locking
	// the individual tag's mutex. We do this *before* locking the global
	// subscription mutex to maintain a consistent lock order and prevent deadlocks.
	tag.valMu.RLock()
	cleanTag := Tag{
		Name:        tag.Name,
		Value:       tag.Value,
		Alias:       tag.Alias,
		TypeInfo:    tag.TypeInfo,
		Description: tag.Description,
		Forced:      tag.Forced,
		ForceValue:  tag.ForceValue,
		Constant:    tag.Constant,
		Retain:      tag.Retain,
	}
	tag.valMu.RUnlock()

	// Now, lock the subscription map and launch a single goroutine to handle all notifications for this update.
	// This is much more efficient than launching one goroutine per subscriber.
	db.subMu.RLock()
	defer db.subMu.RUnlock()

	if subscriptions, ok := db.subscriptions[cleanTag.Name]; ok {
		go func(subs map[uint64]chan Tag, t Tag) {
			for _, ch := range subs {
				select {
				case ch <- t: // Non-blocking send
				default:
					// Channel is full, drop update to avoid blocking.
				}
			}
		}(subscriptions, cleanTag)
	}
}

func contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}

// checkSubrange validates if a value is within the min/max bounds.
func checkSubrange(value, min, max interface{}) error {
	if min == nil && max == nil {
		return nil // No range defined.
	}

	// Use reflection to handle different numeric types without a massive switch case for each combination.
	val := reflect.ValueOf(value)
	minVal := reflect.ValueOf(min)
	maxVal := reflect.ValueOf(max)

	// Handle floating point types
	if val.Kind() >= reflect.Float32 && val.Kind() <= reflect.Float64 {
		v := val.Float()
		if minVal.IsValid() && min != nil {
			m, ok := minVal.Interface().(float64)
			if !ok { // Try to convert if types don't match exactly (e.g. REAL vs LREAL)
				if minVal.CanFloat() {
					m = minVal.Float()
				} else {
					return fmt.Errorf("min value type (%T) is not compatible with value type (%T)", min, value)
				}
			}
			if v < m {
				return fmt.Errorf("value %v is less than minimum %v", v, m)
			}
		}
		if maxVal.IsValid() && max != nil {
			m, ok := maxVal.Interface().(float64)
			if !ok {
				if maxVal.CanFloat() {
					m = maxVal.Float()
				} else {
					return fmt.Errorf("max value type (%T) is not compatible with value type (%T)", max, value)
				}
			}
			if v > m {
				return fmt.Errorf("value %v is greater than maximum %v", v, m)
			}
		}
		return nil
	}

	// Handle signed integer types
	if val.Kind() >= reflect.Int && val.Kind() <= reflect.Int64 {
		v := val.Int()
		if minVal.IsValid() && min != nil {
			m, ok := minVal.Interface().(int64)
			if !ok {
				if minVal.CanInt() {
					m = minVal.Int()
				} else {
					return fmt.Errorf("min value type (%T) is not compatible with value type (%T)", min, value)
				}
			}
			if v < m {
				return fmt.Errorf("value %v is less than minimum %v", v, m)
			}
		}
		if maxVal.IsValid() && max != nil {
			m, ok := maxVal.Interface().(int64)
			if !ok {
				if maxVal.CanInt() {
					m = maxVal.Int()
				} else {
					return fmt.Errorf("max value type (%T) is not compatible with value type (%T)", max, value)
				}
			}
			if v > m {
				return fmt.Errorf("value %v is greater than maximum %v", v, m)
			}
		}
		return nil
	}

	// Handle unsigned integer types
	if val.Kind() >= reflect.Uint && val.Kind() <= reflect.Uint64 {
		v := val.Uint()
		if minVal.IsValid() && min != nil {
			m, ok := minVal.Interface().(uint64)
			if !ok {
				if minVal.CanUint() {
					m = minVal.Uint()
				} else {
					return fmt.Errorf("min value type (%T) is not compatible with value type (%T)", min, value)
				}
			}
			if v < m {
				return fmt.Errorf("value %v is less than minimum %v", v, m)
			}
		}
		// No max check for unsigned, as it's less common and adds complexity. Can be added if needed.
		return nil
	}

	return nil // Not a numeric type we can range check
}

// generateDirectAddress attempts to generate an IEC direct address for a given Tag.
// It returns the direct address string and true if successful, otherwise an empty string and false.
func generateDirectAddress(tag *Tag) (string, bool) {
	// Only generate direct addresses for tags that look like PLC memory addresses
	// (e.g., I.B[0], Q.R[100], M.W[254])
	re := regexp.MustCompile(`^([IQM])\.([BWDLR])\[(\d+)]$`)
	matches := re.FindStringSubmatch(tag.Name)
	if len(matches) != 4 {
		return "", false // Not a recognized symbolic array format for direct addressing
	}

	areaPrefix := matches[1]
	typeChar := matches[2]
	index, _ := strconv.Atoi(matches[3])

	return generateDirectAddressForElement(areaPrefix, typeChar, tag.TypeInfo.ElementType, index)
}

// generateDirectAddressForElement generates an IEC direct address based on its components.
func generateDirectAddressForElement(areaChar, typeChar string, elementType DataType, index int) (string, bool) {
	size, addressable := getPlcTypeSize(elementType)
	if !addressable {
		return "", false // Cannot generate direct address for this element type
	}

	var prefix string
	switch areaChar {
	case "I":
		prefix = "%I"
	case "Q":
		prefix = "%Q"
	case "M":
		prefix = "%M"
	default:
		return "", false
	}

	switch elementType {
	case TypeBOOL:
		byteOffset := index / 8
		bitOffset := index % 8
		return fmt.Sprintf("%sX%d.%d", prefix, byteOffset, bitOffset), true
	case TypeBYTE, TypeSINT, TypeUSINT, TypeWORD, TypeINT, TypeUINT, TypeDWORD, TypeDINT, TypeUDINT, TypeREAL, TypeLWORD, TypeLINT, TypeULINT, TypeLREAL:
		// For byte, word, dword, lword types, the address is the byte offset.
		return fmt.Sprintf("%s%s%d", prefix, typeChar, index*size), true
	default:
		return "", false
	}
}

// getPlcTypeSize returns the size in bytes of a given DataType, and true if it's addressable.
func getPlcTypeSize(dataType DataType) (int, bool) {
	switch dataType {
	case TypeBOOL:
		return 1, true // A BOOL typically occupies 1 bit, but for byte addressing, it's part of a byte.
	case TypeBYTE, TypeSINT, TypeUSINT:
		return 1, true
	case TypeWORD, TypeINT, TypeUINT:
		return 2, true
	case TypeDWORD, TypeDINT, TypeUDINT, TypeREAL:
		return 4, true
	case TypeLWORD, TypeLINT, TypeULINT, TypeLREAL:
		return 8, true
	case TypeSTRING, TypeWSTRING:
		// Variable length types are not simply addressable by byte offset in IEC direct addressing.
		return 0, false
	default:
		return 0, false
	}
}

// persistentTag is an unexported struct used as a data transfer object
// for serializing and deserializing tags to and from a persistence file.
type persistentTag struct {
	Name     string    `json:"Name"`
	TypeInfo *TypeInfo `json:"TypeInfo"`
	Value    any       `json:"Value"`
}

// WriteTagsToFile iterates through the database and writes each tag's name
// and current value to a file.
// This function is optimized to reduce memory allocations by pre-calculating
// the required buffer size and writing directly to a strings.Builder.
func (db *TagDatabase) WriteTagsToFile(filePath string) error {
	// Pre-allocate a slice to hold the lines. This avoids repeated allocations
	// during the Range loop. We can't know the exact size if tags are added/removed
	// concurrently, but it's a good starting point.
	lines := make([]string, 0, 1024) // Start with a reasonable capacity.
	estimatedSize := 0

	db.tags.Range(func(key, value interface{}) bool {
		tag := value.(*Tag)
		tag.valMu.RLock()
		defer tag.valMu.RUnlock()

		// Per documentation, only write tags with the Retain flag.
		// Constants are not persisted unless also marked as Retain.
		if !tag.IsRetain() {
			return true // Continue to the next tag.
		}

		// Create a serializable representation of the tag.
		pTag := persistentTag{
			Name:     tag.Name,
			TypeInfo: tag.TypeInfo,
			Value:    tag.GetValue(), // Use GetValue to respect forcing if needed, though usually not for persistence.
		}

		// Marshal the entire persistentTag struct to JSON for a complete representation.
		jsonData, err := json.Marshal(pTag)
		if err != nil {
			// Optionally log the error, but continue to the next tag.
			return true
		}

		lines = append(lines, string(jsonData))
		estimatedSize += len(jsonData) + 1 // +1 for the newline character
		return true
	})

	// Sort lines for a consistent file output, which is good for debugging and version control.
	sort.Strings(lines)

	// Use a strings.Builder for efficient string concatenation.
	var builder strings.Builder
	builder.Grow(estimatedSize) // Pre-allocate memory.
	builder.WriteString(strings.Join(lines, "\n"))

	return os.WriteFile(filePath, []byte(builder.String()), 0666)
}

// ReadTagsFromFile reads a file of tag values, parses each line,
// and updates the corresponding tag in the database.
func (db *TagDatabase) ReadTagsFromFile(filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		// If the file doesn't exist, it's not an error (e.g., first run).
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	lines := strings.Split(string(data), "\n")
	var errorList []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Each line is now a self-contained JSON object representing a persistentTag.
		var pTag persistentTag
		if err := json.Unmarshal([]byte(line), &pTag); err != nil {
			errorList = append(errorList, fmt.Sprintf("line error: failed to unmarshal JSON: %v", err))
			continue
		}

		tagName := pTag.Name
		valueData := pTag.Value

		// Get the tag to determine its expected data type.
		val, found := db.tags.Load(tagName)
		if !found {
			// If the tag doesn't exist in the database, we can't load its value.
			// This is expected if the application's tag configuration has changed.
			continue
		}
		tag := val.(*Tag)

		// Convert the string value from the file back to the tag's native type.
		var newValue interface{}
		var parseErr error

		_, isUDT := newUDTInstance(tag.TypeInfo.DataType)

		if isUDT {
			// For UDTs, we unmarshal the JSON data back into the existing tag's value pointer.
			tag.valMu.Lock()
			// The value from JSON is a map[string]interface{}, so we re-marshal and unmarshal.
			udtJSON, _ := json.Marshal(valueData)
			if jsonErr := json.Unmarshal(udtJSON, tag.Value); jsonErr != nil {
				parseErr = fmt.Errorf("failed to process UDT data for '%s': %w", tagName, jsonErr)
			}
			tag.valMu.Unlock()
		} else if tag.TypeInfo.DataType == TypeARRAY {
			// For arrays, convert the []interface{} from JSON into a strongly-typed slice.
			if genericSlice, ok := valueData.([]interface{}); ok {
				if elemGoType, ok := getGoType(tag.TypeInfo.ElementType); ok {
					newSlice := reflect.MakeSlice(reflect.SliceOf(elemGoType), len(genericSlice), len(genericSlice))
					for i, v := range genericSlice {
						if convertedVal, err := convertTo(v, elemGoType); err == nil {
							newSlice.Index(i).Set(reflect.ValueOf(convertedVal))
						} else {
							parseErr = fmt.Errorf("error converting element %d for array tag '%s': %w", i, tagName, err)
							break
						}
					}
					if parseErr == nil {
						newValue = newSlice.Interface()
					}
				}
			}
		} else {
			// It's a primitive type.
			newValue, parseErr = parseValueToType(fmt.Sprintf("%v", valueData), tag.TypeInfo.DataType)
		}

		if parseErr != nil {
			errorList = append(errorList, fmt.Sprintf("line error for tag '%s': %v", tagName, parseErr))
		}

		// For UDTs, the value is updated by reference, so we don't call SetTagValue.
		// For primitives and arrays, newValue will be non-nil.
		if newValue != nil {
			if err := db.SetTagValue(tagName, newValue); err != nil {
				errorList = append(errorList, fmt.Sprintf("set value error for tag '%s': %v", tagName, err))
			}
		} // Continue to the next line even if an error occurred on this one.
	}

	if len(errorList) > 0 {
		return fmt.Errorf("encountered %d error(s) while reading tags file:\n- %s", len(errorList), strings.Join(errorList, "\n- "))
	}

	return nil
}

// parseValueToType converts a string value to a specific DataType. It returns the converted value and an error if parsing fails.
func parseValueToType(valueStr string, dataType DataType) (interface{}, error) {
	switch dataType {
	case TypeBOOL:
		b, err := strconv.ParseBool(valueStr)
		return plc.BOOL(b), err
	case TypeSINT:
		i, err := strconv.ParseInt(valueStr, 10, 8)
		return plc.SINT(i), err
	case TypeINT:
		i, err := strconv.ParseInt(valueStr, 10, 16)
		return plc.INT(i), err
	case TypeDINT:
		i, err := strconv.ParseInt(valueStr, 10, 32)
		return plc.DINT(i), err
	case TypeLINT:
		i, err := strconv.ParseInt(valueStr, 10, 64)
		return plc.LINT(i), err
	case TypeUSINT, TypeBYTE:
		i, err := strconv.ParseUint(valueStr, 10, 8)
		return plc.USINT(i), err
	case TypeUINT, TypeWORD:
		i, err := strconv.ParseUint(valueStr, 10, 16)
		return plc.UINT(i), err
	case TypeUDINT, TypeDWORD:
		i, err := strconv.ParseUint(valueStr, 10, 32)
		return plc.UDINT(i), err
	case TypeULINT, TypeLWORD:
		i, err := strconv.ParseUint(valueStr, 10, 64)
		return plc.ULINT(i), err
	case TypeREAL:
		f, err := strconv.ParseFloat(valueStr, 32)
		return plc.REAL(f), err
	case TypeLREAL:
		f, err := strconv.ParseFloat(valueStr, 64)
		return plc.LREAL(f), err
	case TypeSTRING:
		return plc.STRING(valueStr), nil
	default:
		return nil, fmt.Errorf("unsupported data type '%s' for parsing from file", dataType)
	}
}

// getGoType maps our DataType string back to a Go reflect.Type.
func getGoType(dataType DataType) (reflect.Type, bool) {
	// This is the reverse of typeToDataTypeMap.
	// This is less efficient but only used during file loading.
	for t, dt := range typeToDataTypeMap {
		if dt == dataType {
			return t, true
		}
	}
	return nil, false
}

// convertTo handles the conversion of values (often float64 from JSON) to the target PLC type.
func convertTo(value interface{}, targetType reflect.Type) (interface{}, error) {
	sourceValue := reflect.ValueOf(value)

	// If types are directly assignable, no conversion needed.
	if sourceValue.Type().AssignableTo(targetType) {
		return sourceValue.Convert(targetType).Interface(), nil
	}

	// Handle numeric conversions, especially from float64 which is JSON's default for numbers.
	if sourceValue.Kind() == reflect.Float64 {
		floatVal := sourceValue.Float()
		switch targetType.Kind() {
		case reflect.Int8:
			return plc.SINT(floatVal), nil
		case reflect.Int16:
			return plc.INT(floatVal), nil
		case reflect.Int32:
			return plc.DINT(floatVal), nil
		case reflect.Int64:
			return plc.LINT(floatVal), nil
		case reflect.Uint8:
			return plc.USINT(floatVal), nil
		case reflect.Uint16:
			return plc.UINT(floatVal), nil
		case reflect.Uint32:
			return plc.UDINT(floatVal), nil
		case reflect.Uint64:
			return plc.ULINT(floatVal), nil
		case reflect.Float32:
			return plc.REAL(floatVal), nil
		case reflect.Float64:
			return plc.LREAL(floatVal), nil
		}
	}

	// Handle string conversions
	if sourceValue.Kind() == reflect.String {
		strVal := sourceValue.String()
		switch targetType.Kind() {
		case reflect.String:
			// This assumes plc.STRING, plc.WSTRING etc. are type aliases for string
			return reflect.ValueOf(strVal).Convert(targetType).Interface(), nil
		}
	}

	// If no specific conversion rule applies, try a direct conversion.
	if sourceValue.Type().ConvertibleTo(targetType) {
		return sourceValue.Convert(targetType).Interface(), nil
	}

	return nil, fmt.Errorf("cannot convert type %T to %s", value, targetType.Name())
}

// getDataType maps a Go reflect.Type to our DataType string Constant.
func getDataType(t reflect.Type) (DataType, bool) {
	// Check if the type implements the UDT interface.
	// We must check for this first.
	udtInterface := reflect.TypeOf((*UDT)(nil)).Elem()
	if t.Implements(udtInterface) {
		// For UDTs, the DataType is determined by the instance's TypeName() method.
		// We need to create an instance to call the method.
		// The type `t` could be a pointer, so we handle that.
		if t.Kind() == reflect.Ptr {
			t = t.Elem()
		}
		instance := reflect.New(t).Interface()
		if udt, ok := instance.(UDT); ok {
			return udt.TypeName(), true
		}
		return "", false
	}

	// For primitive types, look up the mapping in our pre-initialized map.
	if dt, ok := typeToDataTypeMap[t]; ok {
		return dt, true
	}

	// Check for slice types to identify arrays.
	if t.Kind() == reflect.Slice { // Corrected from TypeARRAY to honeycomb.TypeARRAY
		return TypeARRAY, true
	}

	return "", false
}

// typeToDataTypeMap stores the mapping from Go's reflect.Type to our custom DataType.
// It's initialized once to avoid repeated reflect.TypeOf() calls in getDataType.
var typeToDataTypeMap = make(map[reflect.Type]DataType)

// PopulateDatabaseFromVariables uses reflection to inspect the global I, Q, and M
// address spaces and populates the provided database with corresponding tags.
func PopulateDatabaseFromVariables(db *TagDatabase) error {
	addressSpaces := map[string]interface{}{
		"I": plc.I,
		"Q": plc.Q,
		"M": plc.M,
	}

	for prefix, space := range addressSpaces {
		v := reflect.ValueOf(space)
		t := v.Type()

		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			fieldType := t.Field(i)

			if field.Kind() == reflect.Array {
				// This logic now creates a single ARRAY tag instead of individual element tags.
				elemType := field.Type().Elem()
				var elementType DataType

				// Special handling for M.W which can be ambiguous.
				// We know from the plc library's structure that M.W should be WORD.
				if prefix == "M" && fieldType.Name == "W" {
					elementType = TypeWORD
				} else {
					// For all other fields, infer the type normally.
					var ok bool
					elementType, ok = getDataType(elemType)
					if !ok {
						continue // Skip types we don't have a mapping for
					}
				}

				tagName := fmt.Sprintf("%s.%s", prefix, fieldType.Name)

				// The Go reflection for an array (e.g., [255]bool) is not a slice.
				// We need to convert it to a slice to store it in our ARRAY tag.
				slice := reflect.MakeSlice(reflect.SliceOf(elemType), field.Len(), field.Len())
				reflect.Copy(slice, field)

				tag := &Tag{
					Name:  tagName,
					Value: slice.Interface(),
					TypeInfo: &TypeInfo{
						DataType:    TypeARRAY,
						ElementType: elementType,
					},
				}

				if err := db.AddTag(tag); err != nil {
					return fmt.Errorf("PopulateDatabaseFromVariables: error adding tag '%s': %w", tagName, err)
				}

				// After adding the base array tag, iterate through its elements to
				// generate and store direct address mappings for each one.
				for j := 0; j < field.Len(); j++ {
					elementSymbolicName := fmt.Sprintf("%s[%d]", tagName, j)
					if directAddr, ok := generateDirectAddressForElement(prefix, fieldType.Name, elementType, j); ok {
						// Store the mapping from the direct address to the symbolic element name.
						db.directAddressMap.Store(directAddr, elementSymbolicName)
					}
				}
			}
		}
	}
	return nil
}

// getNestedField handles the logic for accessing a field within a UDT.
// It returns a temporary, read-only Tag representation of the field.
func (db *TagDatabase) getNestedField(fullName string) (Tag, error) { // Corrected from TypeARRAY to honeycomb.TypeARRAY
	// Find the first dot to separate the base path from the field path.
	// This correctly handles paths like "MyArray[0].Field" and "MyUDT.Field".
	dotIndex := strings.Index(fullName, ".")
	if dotIndex == -1 {
		return Tag{}, fmt.Errorf("getNestedField: invalid nested tag name format '%s'", fullName)
	}

	basePath := fullName[:dotIndex]
	fieldPath := fullName[dotIndex+1:]

	// STEP 1: Get the base UDT instance.
	// This could be a top-level UDT or an element from an array of UDTs.
	// We use getTagValueRecursive because it can resolve both simple names and array access.
	udtInstance, err := db.getTagValueRecursive(basePath, 0)
	if err != nil {
		return Tag{}, fmt.Errorf("getNestedField: could not resolve base path '%s': %w", basePath, err)
	}

	// STEP 2: Traverse the field path on the UDT instance.
	// The getFieldFromStruct helper function will walk the dot-separated path (e.g., "Config.Speed").
	fieldValue, err := getFieldFromStruct(udtInstance, fieldPath)
	if err != nil {
		return Tag{}, fmt.Errorf("getNestedField: could not get field '%s' from base '%s': %w", fieldPath, basePath, err)
	}

	// STEP 3: Create a temporary Tag representation of the nested field.
	// This is a read-only representation used for the return value.
	fieldDataType, ok := getDataType(reflect.TypeOf(fieldValue))
	if !ok {
		// This case is unlikely if the UDT is well-defined, but it's a good safeguard.
		return Tag{}, fmt.Errorf("getNestedField: could not determine data type for field '%s'", fieldPath)
	}

	// The returned Tag is a temporary struct holding the value and type of the nested field.
	// It does not exist in the main tag database.
	// Create a temporary, read-only Tag representation of the nested field.
	return Tag{
		Name:  fullName,
		Value: fieldValue,
		TypeInfo: &TypeInfo{
			DataType: fieldDataType,
		},
	}, nil
}

// setSimpleTagValue is the internal, non-recursive implementation for setting a top-level tag's value.
// setSimpleTagValue is the internal, non-recursive implementation for setting a top-level tag's value. It is the base case for recursive set operations.
func (db *TagDatabase) setSimpleTagValue(name string, value interface{}) error {
	val, found := db.tags.Load(name)
	if !found {
		return fmt.Errorf("setTagValue: tag '%s' not found in database", name)
		//return fmt.Errorf("setSimpleTagValue: tag '%s' not found in database", name)
	}
	tag := val.(*Tag)

	// Use the tag's own SetValue method to perform type checking.
	if err := tag.SetValue(value); err != nil {
		return err
	}

	// No need to update the map, as we modified the struct via pointer.
	// Notify any subscribers about the change
	db.notifySubscribers(tag)

	return nil
}

func getFieldFromStruct(udtInstance interface{}, fieldPath string) (interface{}, error) {
	parts := strings.Split(fieldPath, ".")
	currentValue := reflect.ValueOf(udtInstance)

	for _, fieldName := range parts {
		for currentValue.Kind() == reflect.Ptr {
			currentValue = currentValue.Elem()
		}

		if currentValue.Kind() != reflect.Struct {
			return nil, fmt.Errorf("cannot access field '%s' on non-struct type", fieldName)
		}

		currentValue = currentValue.FieldByName(fieldName)
		if !currentValue.IsValid() {
			return nil, fmt.Errorf("field '%s' not found in struct", fieldName)
		}
	}

	return currentValue.Interface(), nil
}

// setNestedField handles writing a value to a field within a UDT or an element of an array of UDTs.
// The `basePath` can be a simple tag name ("MyUDT") or an array element access ("MyArray[1]").
// The `fieldPath` is the dot-separated path to the field to set (e.g., "Config.Speed").
func (db *TagDatabase) setNestedField(basePath string, value interface{}, fieldPath string) (err error) {
	var baseTag *Tag
	var targetStruct reflect.Value

	if strings.Contains(basePath, "[") { // e.g., "MyArray[1]"
		// This is an access to a UDT inside an array.
		// We must parse the access string to get the parent array tag and the element index.
		var index int
		var err error
		baseTag, index, err = db.parseArrayAccess(basePath)
		if err != nil {
			return fmt.Errorf("setNestedField: failed to parse array access '%s': %w", basePath, err)
		}

		// Lock the parent array tag for the entire operation.
		baseTag.valMu.Lock()
		defer func() {
			baseTag.valMu.Unlock()
			if err == nil {
				db.notifySubscribers(baseTag)
			}
		}()

		// Get the slice value from the parent tag.
		sliceVal := reflect.ValueOf(baseTag.Value)
		if sliceVal.Kind() != reflect.Slice {
			return fmt.Errorf("setNestedField: value of tag '%s' is not a slice", baseTag.Name)
		}
		if index < 0 || index >= sliceVal.Len() {
			return fmt.Errorf("setNestedField: index %d out of bounds for array tag '%s' with length %d", index, baseTag.Name, sliceVal.Len())
		}

		// Get the element directly from the slice. This is a pointer to the original data.
		targetStruct = sliceVal.Index(index)

	} else { // e.g., "MyUDT"
		val, found := db.tags.Load(basePath)
		if !found {
			return fmt.Errorf("setNestedField: base tag '%s' not found in database", basePath)
		}
		baseTag = val.(*Tag)

		// Lock the UDT tag for the operation.
		baseTag.valMu.Lock()
		defer func() {
			baseTag.valMu.Unlock()
			if err == nil {
				db.notifySubscribers(baseTag)
			}
		}()

		targetStruct = reflect.ValueOf(baseTag.Value)
	}

	fieldNames := strings.Split(fieldPath, ".")
	currentValue := targetStruct
	if currentValue.Kind() == reflect.Ptr {
		currentValue = currentValue.Elem()
	}

	for i, fieldName := range fieldNames[:len(fieldNames)-1] { // Loop until the second-to-last part.
		currentValue = currentValue.FieldByName(fieldName) // Get the field, which should be a pointer.
		if currentValue.Kind() != reflect.Ptr || currentValue.Elem().Kind() != reflect.Struct {
			return fmt.Errorf("setNestedField: cannot set field on non-UDT tag '%s' at path '%s'", basePath, strings.Join(fieldNames[:i+1], "."))
		}
		currentValue = currentValue.Elem() // Dereference the pointer to get the struct for the next iteration.
	}

	fieldToSet := currentValue.FieldByName(fieldNames[len(fieldNames)-1])
	if !fieldToSet.IsValid() {
		return fmt.Errorf("setNestedField: field '%s' not found in UDT '%s'", fieldNames[len(fieldNames)-1], basePath)
	}
	if !fieldToSet.CanSet() {
		return fmt.Errorf("setNestedField: field '%s' in UDT '%s' is not settable (it may not be exported)", fieldNames[len(fieldNames)-1], basePath)
	}

	incomingValue := reflect.ValueOf(value)
	expectedDataType, _ := getDataType(fieldToSet.Type())

	if enumValues, isEnum := getEnumValues(expectedDataType); isEnum {
		strValue, ok := value.(string)
		if !ok {
			return fmt.Errorf("setNestedField: value for enum field '%s' must be a string", fieldNames[len(fieldNames)-1])
		}
		if !contains(enumValues, strValue) {
			return fmt.Errorf("setNestedField: invalid value '%s' for enum field '%s'", strValue, fieldNames[len(fieldNames)-1])
		}
		fieldToSet.Set(incomingValue)
	} else {
		incomingDataType, ok := getDataType(incomingValue.Type())
		if !ok {
			return fmt.Errorf("setNestedField: value for field '%s' has an unsupported type: %T", fieldNames[len(fieldNames)-1], value)
		}
		if incomingDataType != expectedDataType {
			return fmt.Errorf("setNestedField: type mismatch for field '%s', expects DataType %s but got %s", fieldNames[len(fieldNames)-1], expectedDataType, incomingDataType)
		}

		if incomingValue.Type().AssignableTo(fieldToSet.Type()) {
			fieldToSet.Set(incomingValue)
		}
	}

	return nil
}

// parseArrayAccess parses a tag name with array access (e.g., "MyArr[1,2]")
// and returns the base tag and the calculated flat index.
func (db *TagDatabase) parseArrayAccess(fullName string) (*Tag, int, error) {
	openBracket := strings.LastIndex(fullName, "[")
	if openBracket == -1 {
		return nil, -1, fmt.Errorf("parseArrayAccess: invalid array access format '%s'", fullName)
	}

	baseTagName := fullName[:openBracket]
	indicesStr := fullName[openBracket+1 : len(fullName)-1]

	// Parse comma-separated indices
	indexParts := strings.Split(indicesStr, ",")
	indices := make([]int, len(indexParts))
	for i, part := range indexParts {
		idx, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, -1, fmt.Errorf("parseArrayAccess: invalid array index '%s' in '%s'", part, fullName)
		}
		indices[i] = idx
	}

	val, found := db.tags.Load(baseTagName)
	if !found {
		return nil, -1, fmt.Errorf("parseArrayAccess: base array tag '%s' not found", baseTagName)
	}
	baseTag := val.(*Tag)

	// Calculate the flat index
	flatIndex, err := calculateFlatIndex(baseTag.TypeInfo.Dimensions, indices)
	if err != nil {
		return nil, -1, fmt.Errorf("parseArrayAccess: for tag '%s', %w", fullName, err)
	}

	return baseTag, flatIndex, nil
}

// calculateFlatIndex computes the 1D index from multi-dimensional indices.
func calculateFlatIndex(dimensions []int, indices []int) (int, error) {
	// If dimensions are not specified, treat it as a simple 1D array.
	if len(dimensions) == 0 {
		if len(indices) != 1 {
			return -1, fmt.Errorf("incorrect number of indices provided for a 1D array; expected 1, got %d", len(indices))
		}
		// For a 1D array, the index is simply the first (and only) index provided.
		flatIndex := indices[0]
		if flatIndex < 0 {
			return -1, fmt.Errorf("index %d is out of bounds", flatIndex)
		}
		return flatIndex, nil
	}

	if len(dimensions) != len(indices) {
		return -1, fmt.Errorf("incorrect number of indices provided; expected %d, got %d", len(dimensions), len(indices))
	}

	flatIndex := 0
	multiplier := 1
	for i := len(dimensions) - 1; i >= 0; i-- {
		if indices[i] < 0 || indices[i] >= dimensions[i] {
			return -1, fmt.Errorf("index %d is out of bounds for dimension %d (size %d)", indices[i], i, dimensions[i])
		}
		flatIndex += indices[i] * multiplier
		multiplier *= dimensions[i]
	}

	return flatIndex, nil
}

// getArrayElementValue retrieves an element from a tag's slice value after all checks.
func getArrayElementValue(baseTag *Tag, index int) (interface{}, error) {
	baseTag.valMu.RLock()
	defer baseTag.valMu.RUnlock()

	if baseTag.TypeInfo.DataType != TypeARRAY { // Corrected from TypeARRAY to honeycomb.TypeARRAY
		return nil, fmt.Errorf("getArrayElementValue: tag '%s' is not an array", baseTag.Name)
	}

	sliceVal := reflect.ValueOf(baseTag.Value)
	if sliceVal.Kind() != reflect.Slice {
		return nil, fmt.Errorf("getArrayElementValue: value of tag '%s' is not a slice", baseTag.Name)
	}

	if index < 0 || index >= sliceVal.Len() {
		return nil, fmt.Errorf("getArrayElementValue: index %d out of bounds for array tag '%s' with length %d", index, baseTag.Name, sliceVal.Len())
	}

	return sliceVal.Index(index).Interface(), nil
}

// setArrayElementValue writes a value to an element of a tag's slice value.
func setArrayElementValue(baseTag *Tag, index int, value interface{}) error {
	baseTag.valMu.Lock()
	defer baseTag.valMu.Unlock()

	if baseTag.TypeInfo.DataType != TypeARRAY { // Corrected from TypeARRAY to honeycomb.TypeARRAY
		return fmt.Errorf("setArrayElementValue: tag '%s' is not an array", baseTag.Name)
	}

	// Type check the incoming value against the array's ElementType.
	incomingDataType, ok := getDataType(reflect.TypeOf(value))
	if !ok || incomingDataType != baseTag.TypeInfo.ElementType {
		return fmt.Errorf("setArrayElementValue: type mismatch for array '%s', expects element type %s but got %s", baseTag.Name, baseTag.TypeInfo.ElementType, incomingDataType)
	}

	sliceVal := reflect.ValueOf(baseTag.Value)
	if index < 0 || index >= sliceVal.Len() {
		return fmt.Errorf("setArrayElementValue: index %d out of bounds for array tag '%s' with length %d", index, baseTag.Name, sliceVal.Len())
	}

	// Set the value at the specified index.
	sliceVal.Index(index).Set(reflect.ValueOf(value))

	return nil
}

// NewValueFromDataType creates a pointer to a zero value of the given DataType.
// This is particularly useful for unmarshaling JSON into a strongly-typed variable.
// For primitive types, it returns a pointer to the corresponding plc type (e.g., *plc.DINT).
// For UDTs, it returns a pointer to a new instance of the UDT struct (e.g., *MotorData).
func NewValueFromDataType(dataType DataType) (interface{}, error) {
	// First, check if it's a registered UDT.
	if udtInstance, isUDT := newUDTInstance(dataType); isUDT {
		return udtInstance, nil
	}

	// Next, check if it's a primitive Go type.
	if goType, ok := getGoType(dataType); ok {
		// Create a new pointer to a value of that type.
		return reflect.New(goType).Interface(), nil
	}

	// If it's an ENUM, it's fundamentally a string.
	if _, isEnum := getEnumValues(dataType); isEnum {
		var s string
		return &s, nil
	}

	// If the type is not found, return an error.
	return nil, fmt.Errorf("unrecognized or unsupported DataType '%s'", dataType)
}

// Dereference takes an interface that is expected to be a pointer and returns
// the value it points to. If the input is not a pointer, it returns the input itself.
// This is a helper function to simplify getting the underlying value after unmarshaling
// into a pointer, as is common in the `handleSetTagValue` HTTP handler.
func Dereference(ptr interface{}) interface{} {
	if ptr == nil {
		return nil
	}

	val := reflect.ValueOf(ptr)

	// If the interface holds a pointer, dereference it.
	if val.Kind() == reflect.Ptr {
		// If the pointer is nil, return nil.
		if val.IsNil() {
			return nil
		}
		// Otherwise, return the element it points to.
		return val.Elem().Interface()
	}

	// If it's not a pointer, return the value as is.
	return ptr
}

// --- Internal Network Server Implementation ---

// tagServer holds the server's TagDatabase and configuration. It is not exported.
type tagServer struct {
	db          *TagDatabase
	validTokens []string
}

// tagHandler is the main router for the `/tags/` endpoint.
func (ts *tagServer) tagHandler(w http.ResponseWriter, r *http.Request) {
	// All requests to this handler have `/tags/` as a prefix.
	// We trim it to get the actual tag name being requested.
	tagName := strings.TrimPrefix(r.URL.Path, "/tags/")
	if tagName == "" {
		http.Error(w, "Tag name is required.", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		ts.handleGetTagValue(w, r, tagName)
	case http.MethodPut:
		ts.handleSetTagValue(w, r, tagName)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// authMiddleware protects server endpoints with Bearer Token authentication.
func (ts *tagServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authorization header is required", http.StatusUnauthorized)
			return
		}
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			http.Error(w, "Authorization header format must be Bearer {token}", http.StatusUnauthorized)
			return
		}
		token := parts[1]
		isValid := false
		for _, validToken := range ts.validTokens {
			if token == validToken {
				isValid = true
				break
			}
		}
		if !isValid {
			http.Error(w, "Invalid authentication token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleGetTagValue handles GET requests to read a tag's value.
func (ts *tagServer) handleGetTagValue(w http.ResponseWriter, r *http.Request, tagName string) {
	log.Printf("[Server] GET /tags/%s", tagName)
	value, err := ts.db.GetTagValue(tagName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	response := map[string]interface{}{"value": value}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// handleSetTagValue handles PUT requests to update a tag's value.
func (ts *tagServer) handleSetTagValue(w http.ResponseWriter, r *http.Request, tagName string) {
	log.Printf("[Server] PUT /tags/%s", tagName)
	// Get the base tag to correctly determine the type for unmarshaling,
	// even if the write is to a nested field (e.g., "MyUDT.Field").
	tag, found := ts.db.getBaseTag(tagName)
	if !found {
		http.Error(w, fmt.Sprintf("Tag '%s' not found", tagName), http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}

	// The request body is expected to be a JSON object like {"value": ...}.
	var requestPayload map[string]json.RawMessage
	if err := json.Unmarshal(body, &requestPayload); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	valueJSON, ok := requestPayload["value"]
	if !ok {
		http.Error(w, "Missing 'value' field", http.StatusBadRequest)
		return
	}

	// To correctly unmarshal the value, especially for UDTs, we need a variable
	// of the correct type. We can get this from the tag's current value.
	// For a nested write, we unmarshal into a generic interface{}.
	// For a whole-tag write, we unmarshal into a new instance of the tag's type.
	if strings.Contains(tagName, ".") || strings.Contains(tagName, "[") {
		// Handle nested writes (e.g., "MyUDT.Speed", "MyArray[0].Field").
		// The value is just the raw JSON value. We unmarshal it into a generic interface.
		var value interface{}
		if err := json.Unmarshal(valueJSON, &value); err != nil {
			http.Error(w, "Invalid JSON value for nested write", http.StatusBadRequest)
			return
		}
		if err := ts.db.SetTagValue(tagName, value); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		// Handle whole-tag writes (e.g., "MyUDT", "MyDINT").
		newValuePtr, err := NewValueFromDataType(tag.TypeInfo.DataType)
		if err != nil {
			http.Error(w, fmt.Sprintf("Internal server error: could not create instance for type '%s': %v", tag.TypeInfo.DataType, err), http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(valueJSON, newValuePtr); err != nil {
			http.Error(w, "JSON value is not compatible with tag type", http.StatusBadRequest)
			return
		}
		if err := ts.db.SetTagValue(tagName, Dereference(newValuePtr)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "Tag value updated successfully.")
}

// getBaseTag is an internal helper that finds the top-level tag associated with a given name,
// even if the name represents a nested field or array element (e.g., "MyUDT.Field" or "MyArray[0]").
// It returns a pointer to the actual tag in the database, not a copy.
func (db *TagDatabase) getBaseTag(name string) (*Tag, bool) {
	// The base tag name is the part before the first dot or bracket.
	var baseTagName string
	if dotIndex := strings.Index(name, "."); dotIndex != -1 {
		baseTagName = name[:dotIndex]
	} else if bracketIndex := strings.Index(name, "["); bracketIndex != -1 {
		baseTagName = name[:bracketIndex]
	} else {
		baseTagName = name
	}

	if val, found := db.tags.Load(baseTagName); found {
		return val.(*Tag), true
	}
	return nil, false
}
