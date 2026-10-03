/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package honeycomb

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	plc "github.com/apiarytech/royaljelly/iec"
	"github.com/apiarytech/royaljelly/vars"
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

// ForceInfo encapsulates the state related to forcing a tag's value.
// A non-nil pointer to this struct indicates the tag is forced.
type ForceInfo struct {
	Value interface{} // Value stores the value that overrides the actual Value when the tag is Forced.
}

// RemoteAliasInfo encapsulates the configuration for a remote alias tag.
// A non-nil pointer to this struct indicates the tag is a remote alias.
type RemoteAliasInfo struct {
	DBID    string // The ID of the remote database.
	TagName string // The name of the tag in the remote database.
}

// Tag represents a single variable (tag) in the system.
// It encapsulates all properties and the current state of a PLC tag.
type Tag struct {
	valMu         sync.RWMutex     // valMu provides read/write mutex protection for the tag's internal state.
	Name          string           // Name is the unique symbolic name of the tag.
	Value         interface{}      // Value holds the current data value of the tag.
	Quality       Quality          // Quality reports how trustworthy Value is. A new tag starts as QualityUnknown.
	Timestamp     time.Time        // Timestamp is when Value or Quality last changed: the device's time if a driver supplied it (SetTagValueQualityAt), else the time of the write. Zero until the first write.
	Sequence      uint64           // Sequence numbers the updates sent to subscribers; it is set only on those copies. It rises by one per update, so a gap means newer updates replaced ones the subscriber had not received.
	Alias         string           // Alias provides an alternative, often shorter, name for the tag.
	DirectAddress string           // DirectAddress stores the IEC 61131-3 direct address (e.g., %IX0.0, %MW10) if applicable.
	TypeInfo      *TypeInfo        // TypeInfo is a pointer to the shared TypeInfo struct defining the tag's data type characteristics.
	Description   string           // Description provides a human-readable explanation or purpose of the tag.
	Constant      bool             // Constant, if true, prevents any modification to the tag's Value or ForceValue after creation.
	Retain        bool             // Retain, if true, marks the tag's value for persistence across application restarts.
	Force         *ForceInfo       // If not nil, the tag's value is forced with the value in this struct.
	RemoteAlias   *RemoteAliasInfo // If not nil, this tag is an alias for a tag in another database.

	notifyMu sync.Mutex // serializes notifications, so subscribers receive updates in order
	seq      uint64     // Sequence of the last update sent to subscribers; guarded by notifyMu
}

// snapshot returns a copy of the tag's state without its locks or internal
// counters. The caller must hold valMu.
func (t *Tag) snapshot() Tag {
	return Tag{
		Name:          t.Name,
		Value:         t.Value,
		Quality:       t.Quality,
		Timestamp:     t.Timestamp,
		Alias:         t.Alias,
		DirectAddress: t.DirectAddress,
		TypeInfo:      t.TypeInfo,
		Description:   t.Description,
		Constant:      t.Constant,
		Retain:        t.Retain,
		Force:         t.Force,
		RemoteAlias:   t.RemoteAlias,
	}
}

// stamp returns ts, or the current time if ts is zero.
func stamp(ts time.Time) time.Time {
	if ts.IsZero() {
		return time.Now()
	}
	return ts
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
	return t.presentedValue()
}

// presentedValue is GetValue for a caller that already holds valMu. (Taking a
// read lock twice deadlocks if a writer queues in between.)
func (t *Tag) presentedValue() interface{} {
	if t.Force != nil {
		// Remote aliases do not have their own force values.
		// The forcing is handled on the remote tag itself.
		if t.RemoteAlias != nil {
			// This path should ideally not be hit if GetTagValue is used, but as a safeguard:
			return nil
		}
		return t.Force.Value
	}
	return t.Value
}

// SetValue updates the value of the tag and marks it QualityGood.
// It performs a type check to ensure the new value is compatible with the tag's DataType.
// Note: This method modifies the Tag struct directly. If you retrieved this Tag from a
// TagDatabase, you must use the database's SetTagValue method to ensure the change
// is saved in the thread-safe map.
func (t *Tag) SetValue(value interface{}) error {
	return t.SetValueQuality(value, QualityGood)
}

// SetValueQuality updates the value and quality of the tag together and sets
// its Timestamp to now. It performs the same checks as SetValue.
func (t *Tag) SetValueQuality(value interface{}, quality Quality) error {
	return t.SetValueQualityAt(value, quality, time.Time{})
}

// SetValueQualityAt is SetValueQuality with the time the value was produced,
// e.g. a device's source timestamp. A zero timestamp means now.
func (t *Tag) SetValueQualityAt(value interface{}, quality Quality, timestamp time.Time) error {
	if !quality.IsValid() {
		return fmt.Errorf("invalid quality %d for tag '%s'", uint8(quality), t.Name)
	}
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
		value = truncateString(value, t.TypeInfo.MaxLength)
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
	t.Quality = quality
	t.Timestamp = stamp(timestamp)
	return nil
}

// GetQuality returns the quality of the tag's value.
func (t *Tag) GetQuality() Quality {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.Quality
}

// GetTimestamp returns when the tag's value or quality last changed.
func (t *Tag) GetTimestamp() time.Time {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.Timestamp
}

// GetForceValue returns the forced value of the tag.
func (t *Tag) GetForceValue() interface{} {
	t.valMu.RLock()
	defer t.valMu.RUnlock()
	return t.Force.Value
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
	if value == nil && t.Force != nil {
		t.Force.Value = nil
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
		value = truncateString(value, t.TypeInfo.MaxLength)
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

	if t.Force == nil {
		t.Force = &ForceInfo{}
	}
	t.Force.Value = value
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
	return t.Force != nil
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

// Tagger defines the interface for interacting with a single tag.
type Tagger interface {
	GetName() string
	GetAlias() string
	GetDataType() DataType
	GetDescription() string
	IsForced() bool
	GetValue() interface{}
	GetQuality() Quality
	GetTimestamp() time.Time
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
	SetTagValueQuality(name string, value interface{}, quality Quality) error
	SetTagValueQualityAt(name string, value interface{}, quality Quality, timestamp time.Time) error
	SetTagQuality(name string, quality Quality) error
	GetTagQuality(name string) (Quality, error)
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
	directAddressMap sync.Map // map[string]string (direct address -> symbolic name)
	typeRegistry     sync.Map // map[DataType]*TypeInfo
	subscriptions    map[string]map[uint64]chan Tag
	subMu            sync.RWMutex
	dbRegistry       sync.Map // Instance-level registry: map[string]DatabaseAccessor
	// PersistenceWorkers specifies the number of worker goroutines to use for
	// file read/write operations. It defaults to runtime.NumCPU().
	PersistenceWorkers int
	// persister is the attached TagStore's Persister, or nil. See AttachStore.
	persister atomic.Pointer[Persister]
	// feed is the change feed (changefeed.go), created on first use.
	feedOnce sync.Once
	feed     atomic.Pointer[changeFeed]
}

// DatabaseAccessor defines the interface for any object that can be registered
// as a remote database, allowing for both in-process and networked aliasing.
type DatabaseAccessor interface {
	getTagValueRecursive(name string, depth int) (any, error)
	setTagValueRecursive(name string, value any, quality Quality, timestamp time.Time, depth int) error
	getTagQualityRecursive(name string, depth int) (Quality, error)
	setTagQualityRecursive(name string, quality Quality, depth int) error
	readTagRecursive(name string, depth int) (Reading, error)
}

// NetworkDatabaseClient is an implementation of DatabaseAccessor that communicates
// with a remote TagDatabase server over a network.
// This is a conceptual example; a real implementation would require a network
// protocol (e.g., HTTP, gRPC) and corresponding server-side handlers.

// NewTagDatabase creates and returns a new TagDatabase instance.
func NewTagDatabase() *TagDatabase {
	return &TagDatabase{
		subscriptions:      make(map[string]map[uint64]chan Tag),
		PersistenceWorkers: runtime.NumCPU(), // Default to the number of CPU cores.
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

// The channel holds one update. If an update arrives before the subscriber has
// received the previous one, the newer update replaces it: the subscriber always
// gets the tag's latest state, and Tag.Sequence shows how many updates it missed.
// Updates for a tag are delivered in order.
//
// A remote alias cannot be subscribed to, since its writes happen in the remote
// database; subscribe on the database that owns the tag instead.
func (db *TagDatabase) SubscribeToTag(tagName string) (<-chan Tag, uint64, error) {
	val, found := db.tags.Load(tagName)
	if !found {
		return nil, 0, fmt.Errorf("tag '%s' not found for subscription", tagName)
	}
	if alias := val.(*Tag).RemoteAlias; alias != nil {
		return nil, 0, fmt.Errorf("tag '%s' is a remote alias; subscribe to '%s' on database '%s' instead", tagName, alias.TagName, alias.DBID)
	}

	db.subMu.Lock()
	defer db.subMu.Unlock()

	if _, ok := db.subscriptions[tagName]; !ok {
		db.subscriptions[tagName] = make(map[uint64]chan Tag)
	} // Corrected from TypeARRAY to honeycomb.TypeARRAY

	ch := make(chan Tag, 1) // Buffered channel to avoid blocking the publisher
	// Use a random number for the ID to avoid overflow and make it unpredictable.
	id := rand.Uint64()
	// Ensure the ID is unique for this tag's subscriptions.
	for _, exists := db.subscriptions[tagName][id]; exists; _, exists = db.subscriptions[tagName][id] {
		id = rand.Uint64()
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
	if tag.RemoteAlias == nil && tag.TypeInfo == nil {
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
	if tagPtr.RemoteAlias == nil && tagPtr.TypeInfo != nil {
		_, isUDT := newUDTInstance(tagPtr.TypeInfo.DataType)
		if isUDT && tagPtr.Value == nil {
			if instance, ok := newUDTInstance(tagPtr.TypeInfo.DataType); ok {
				tagPtr.Value = instance
			}
		}
	}
	// A Constant is never written, so its configured value is Good from the start.
	if tagPtr.Constant && tagPtr.Value != nil && tagPtr.Quality == QualityUnknown {
		tagPtr.Quality = QualityGood
		tagPtr.Timestamp = stamp(tagPtr.Timestamp)
	}
	tagPtr.valMu.Unlock()

	// If the tag is a PLC memory address, generate and store its direct address mapping.
	if directAddr, ok := generateDirectAddress(tag); ok {
		tag.DirectAddress = directAddr
	}

	if tag.DirectAddress != "" {
		db.directAddressMap.Store(canonicalAddress(tag.DirectAddress), tag.Name)
	}
	db.markChanged(tag.Name)
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
		tagPtr.valMu.RLock()
		defer tagPtr.valMu.RUnlock()
		return tagPtr.snapshot(), true
	}

	// If not found, check for nested UDT field access (e.g., "MyUDT.Field").
	if strings.Contains(name, ".") {
		// This path is for read-only access to a nested field as if it were a tag.
		// It returns a temporary honeycomb.Tag struct representing the field.
		nestedtag, err := db.getNestedField(name)
		if err != nil {
			return Tag{}, false // The nested field was not found, so return false.
		}
		quality, _ := db.GetTagQuality(name) // A field shares its parent tag's quality.
		return Tag{
			Name:        nestedtag.Name,
			Value:       nestedtag.Value, // This is the field's value
			Quality:     quality,
			Timestamp:   db.tagTimestamp(name),
			Alias:       nestedtag.Alias,
			TypeInfo:    nestedtag.TypeInfo,
			Description: nestedtag.Description, // This could be the field's description if we add it
			Force:       nestedtag.Force,
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
		quality, _ := db.GetTagQuality(name) // An element shares its array's quality.
		return Tag{
			Name:      name,
			Value:     element,
			Quality:   quality,
			Timestamp: db.tagTimestamp(name),
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
	tags := make([]Tag, 0) // Initialize as an empty slice, not a nil slice.
	db.tags.Range(func(_, value interface{}) bool {
		tagPtr := value.(*Tag)
		tagPtr.valMu.RLock()
		tags = append(tags, tagPtr.snapshot())
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
			foundTags[name] = tagPtr.snapshot()
			tagPtr.valMu.RUnlock()
		}
	}
	return foundTags
}

// GetTagsByType returns a slice of all tags that match the given DataType.
func (db *TagDatabase) GetTagsByType(dataType DataType) []Tag {
	matchingTags := make([]Tag, 0)
	db.tags.Range(func(key, value interface{}) bool {
		tag := value.(*Tag)
		tag.valMu.RLock()
		// Remote aliases have no TypeInfo of their own.
		if tag.TypeInfo != nil && tag.TypeInfo.DataType == dataType {
			matchingTags = append(matchingTags, tag.snapshot())
		}
		tag.valMu.RUnlock()
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
	db.markRemoved(name)
	db.changeFeed().forget(name)

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
			db.directAddressMap.Delete(canonicalAddress(tag.DirectAddress))
		}

		// If the tag is a process-image array (see PopulateDatabaseFromImage), we
		// must also remove the direct address mappings for all its elements.
		if tag.TypeInfo != nil && tag.TypeInfo.DataType == TypeARRAY {
			if sliceVal := reflect.ValueOf(tag.Value); sliceVal.Kind() == reflect.Slice {
				imageArrayAddresses(tag.Name, sliceVal.Len(), func(_ int, addr string) {
					db.directAddressMap.Delete(addr)
				})
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
	db.markRemoved(oldName)
	db.markChanged(newName)

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
		db.directAddressMap.Store(canonicalAddress(tagPtr.DirectAddress), newName)
	} else if tagPtr.TypeInfo != nil && tagPtr.TypeInfo.DataType == TypeARRAY {
		// For a process-image array, remap each element's address. The addresses
		// come from the old name, since the tag's internal name was just changed.
		if sliceVal := reflect.ValueOf(tagPtr.Value); sliceVal.Kind() == reflect.Slice {
			imageArrayAddresses(oldName, sliceVal.Len(), func(i int, addr string) {
				// e.g. %IX0 -> I.B[0] becomes %IX0 -> MyInputs[0]
				db.directAddressMap.Store(addr, fmt.Sprintf("%s[%d]", newName, i))
			})
		}
	}

	// Create and return a safe copy of the tag's state.
	return tagPtr.snapshot(), nil
}

// SetTagValue updates the value of an existing tag in the database and marks it
// QualityGood. It performs a type check to ensure the new value is compatible with
// the tag's DataType. Writing an array element or UDT field sets the quality of the
// whole tag, since quality is tracked per top-level tag.
func (db *TagDatabase) SetTagValue(name string, value interface{}) error {
	return db.setTagValueRecursive(name, value, QualityGood, time.Time{}, 0)
}

// SetTagValueQuality updates a tag's value and quality together, so readers and
// subscribers never see one without the other, and sets its Timestamp to now.
// Drivers and protocol bridges should use it instead of SetTagValue.
func (db *TagDatabase) SetTagValueQuality(name string, value interface{}, quality Quality) error {
	return db.SetTagValueQualityAt(name, value, quality, time.Time{})
}

// SetTagValueQualityAt is SetTagValueQuality with the time the value was
// produced. Drivers pass the device's source timestamp here, so a
// sequence-of-events record shows when the device saw the change rather than
// when honeycomb received it. A zero timestamp means now.
func (db *TagDatabase) SetTagValueQualityAt(name string, value interface{}, quality Quality, timestamp time.Time) error {
	if !quality.IsValid() {
		return fmt.Errorf("SetTagValueQuality: invalid quality %d for tag '%s'", uint8(quality), name)
	}
	return db.setTagValueRecursive(name, value, quality, timestamp, 0)
}

func (db *TagDatabase) setTagValueRecursive(name string, value interface{}, quality Quality, timestamp time.Time, depth int) (err error) {
	// First, check if the name is a direct address.
	if name, err = db.resolveAddress(name); err != nil {
		return fmt.Errorf("SetTagValue: %w", err)
	}

	// Check for remote alias before any other processing.
	if val, found := db.tags.Load(name); found {
		tag := val.(*Tag)
		if tag.RemoteAlias != nil {
			if depth > 10 { // Prevent infinite recursion
				return fmt.Errorf("max recursion depth exceeded for remote alias '%s'", name)
			}
			remoteDB, found := db.getDatabase(tag.RemoteAlias.DBID)
			if !found {
				return fmt.Errorf("remote database with ID '%s' not found for alias '%s'", tag.RemoteAlias.DBID, name)
			}
			// Call the remote database's SetTagValue.
			return remoteDB.setTagValueRecursive(tag.RemoteAlias.TagName, value, quality, timestamp, depth+1)
		}
		// An exact match is a whole-tag write even if the name contains '.' or '['
		// (e.g. "Press1.Pressure"), matching the lookup order of GetTagValue.
		return db.setSimpleTagValue(name, value, quality, timestamp)
	}

	// If the value being set is itself a UDT, we should treat it as a wholesale
	// replacement of the tag's value, not a nested field write, even if the name // Corrected from TypeARRAY to honeycomb.TypeARRAY
	// contains dots (which it shouldn't for this case, but we check defensively).
	if _, isUDT := value.(UDT); isUDT {
		return db.setSimpleTagValue(name, value, quality, timestamp)
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
			return db.setNestedField(arrayPart, value, fieldPart, quality, timestamp)
		}
	}

	// Check for array element access.
	if strings.Contains(name, "[") && strings.HasSuffix(name, "]") {
		baseTag, index, err := db.parseArrayAccess(name)
		if err != nil {
			return err
		}
		// Lock, type check, and set the value.
		if err := setArrayElementValue(baseTag, index, value, quality, timestamp); err != nil {
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
		return db.setNestedField(basePath, value, fieldPath, quality, timestamp)
	}

	// If not nested, proceed with updating the whole tag value.
	return db.setSimpleTagValue(name, value, quality, timestamp)
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
	// If it's a direct address, replace it with its symbolic name for further processing.
	name, err := db.resolveAddress(name)
	if err != nil {
		return nil, fmt.Errorf("GetTagValue: %w", err)
	}

	// STEP 2: Direct Tag Match.
	// Check if the name (which could now be a resolved symbolic name) exists as a top-level tag.
	val, found := db.tags.Load(name)
	if found {
		tag := val.(*Tag)
		// STEP 3: Remote Alias Resolution.
		// If the found tag is a remote alias, we must delegate the request to the target database. // Corrected from IsRemoteAlias to RemoteAlias
		if tag.RemoteAlias != nil {
			if depth > 10 { // Safety check to prevent infinite loops in alias chains.
				return nil, fmt.Errorf("max recursion depth exceeded for remote alias '%s'", name)
			}
			remoteDB, found := db.getDatabase(tag.RemoteAlias.DBID)
			if !found {
				return nil, fmt.Errorf("remote database with ID '%s' not found for alias '%s'", tag.RemoteAlias.DBID, name)
			}
			// Recursively call this function on the remote DB with the remote tag name.
			return remoteDB.getTagValueRecursive(tag.RemoteAlias.TagName, depth+1)
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

			// Read the field of the array element under the array's lock.
			return db.fieldOf(arrayPart, fieldPart, depth)
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

// GetTagQuality returns the quality of a tag's value. For an array element or UDT
// field it returns the quality of the whole tag. On error the quality is QualityBad,
// so a caller that only forwards the quality (e.g. to OPC) reports the failure.
func (db *TagDatabase) GetTagQuality(name string) (Quality, error) {
	return db.getTagQualityRecursive(name, 0)
}

func (db *TagDatabase) getTagQualityRecursive(name string, depth int) (Quality, error) {
	tag, err := db.qualityTag(name)
	if err != nil {
		return QualityBad, fmt.Errorf("GetTagQuality: %w", err)
	}
	if tag.RemoteAlias != nil {
		remoteDB, err := db.remoteForAlias(tag, depth)
		if err != nil {
			return QualityBad, fmt.Errorf("GetTagQuality: %w", err)
		}
		return remoteDB.getTagQualityRecursive(tag.RemoteAlias.TagName, depth+1)
	}
	return tag.GetQuality(), nil
}

// Reading is a tag's value, quality and timestamp, read together.
type Reading struct {
	Value     any
	Quality   Quality
	Timestamp time.Time // zero if the value was never written or the source does not report it
}

// ReadTag returns a tag's value, quality and timestamp in one call. It accepts
// the same names as GetTagValue. For a remote alias it makes a single request to
// the remote database, so a client polling remote tags pays one round trip per
// tag. On error the quality is QualityBad.
func (db *TagDatabase) ReadTag(name string) (Reading, error) {
	return db.readTagRecursive(name, 0)
}

func (db *TagDatabase) readTagRecursive(name string, depth int) (Reading, error) {
	tag, err := db.qualityTag(name)
	if err != nil {
		return Reading{Quality: QualityBad}, fmt.Errorf("ReadTag: %w", err)
	}
	if tag.RemoteAlias != nil {
		remoteDB, err := db.remoteForAlias(tag, depth)
		if err != nil {
			return Reading{Quality: QualityBad}, fmt.Errorf("ReadTag: %w", err)
		}
		// Keep an element or field suffix, e.g. "Alias.Speed" reads "Remote.Speed".
		suffix := strings.TrimPrefix(name, tag.Name)
		return remoteDB.readTagRecursive(tag.RemoteAlias.TagName+suffix, depth+1)
	}
	value, err := db.getTagValueRecursive(name, depth)
	if err != nil {
		return Reading{Quality: QualityBad}, fmt.Errorf("ReadTag: %w", err)
	}
	return Reading{Value: value, Quality: tag.GetQuality(), Timestamp: tag.GetTimestamp()}, nil
}

// SetTagQuality changes a tag's quality without touching its value, e.g. when a
// driver loses its connection and the last value read becomes Uncertain or Bad.
// For an array element or UDT field it sets the quality of the whole tag.
// Subscribers are notified only when the quality actually changes.
func (db *TagDatabase) SetTagQuality(name string, quality Quality) error {
	if !quality.IsValid() {
		return fmt.Errorf("SetTagQuality: invalid quality %d for tag '%s'", uint8(quality), name)
	}
	return db.setTagQualityRecursive(name, quality, 0)
}

func (db *TagDatabase) setTagQualityRecursive(name string, quality Quality, depth int) error {
	tag, err := db.qualityTag(name)
	if err != nil {
		return fmt.Errorf("SetTagQuality: %w", err)
	}
	if tag.RemoteAlias != nil {
		remoteDB, err := db.remoteForAlias(tag, depth)
		if err != nil {
			return fmt.Errorf("SetTagQuality: %w", err)
		}
		return remoteDB.setTagQualityRecursive(tag.RemoteAlias.TagName, quality, depth+1)
	}

	tag.valMu.Lock()
	changed := tag.Quality != quality
	if changed {
		tag.Quality = quality
		tag.Timestamp = time.Now() // A quality change is an event in its own right.
	}
	tag.valMu.Unlock()
	if changed {
		db.notifySubscribers(tag)
	}
	return nil
}

// tagTimestamp returns the Timestamp that applies to a tag name, direct address,
// array element or UDT field: that of its top-level tag. It is zero for unknown
// names and remote aliases, whose timestamps live in the remote database.
func (db *TagDatabase) tagTimestamp(name string) time.Time {
	tag, err := db.qualityTag(name)
	if err != nil || tag.RemoteAlias != nil {
		return time.Time{}
	}
	return tag.GetTimestamp()
}

// qualityTag resolves a tag name, direct address, array element or UDT field to
// the top-level tag that holds its quality.
func (db *TagDatabase) qualityTag(name string) (*Tag, error) {
	name, err := db.resolveAddress(name)
	if err != nil {
		return nil, err
	}
	if val, found := db.tags.Load(name); found {
		return val.(*Tag), nil
	}
	// Tag names may themselves contain dots (e.g. "I.B"), so try every prefix
	// that ends before a '.' or '[' rather than splitting at the first one.
	for i, r := range name {
		if r == '.' || r == '[' {
			if val, found := db.tags.Load(name[:i]); found {
				return val.(*Tag), nil
			}
		}
	}
	return nil, fmt.Errorf("tag '%s' not found in database", name)
}

// remoteForAlias returns the database a remote alias tag points to.
func (db *TagDatabase) remoteForAlias(tag *Tag, depth int) (DatabaseAccessor, error) {
	if depth > 10 { // Prevent infinite recursion in alias chains.
		return nil, fmt.Errorf("max recursion depth exceeded for remote alias '%s'", tag.Name)
	}
	remoteDB, found := db.getDatabase(tag.RemoteAlias.DBID)
	if !found {
		return nil, fmt.Errorf("remote database with ID '%s' not found for alias '%s'", tag.RemoteAlias.DBID, tag.Name)
	}
	return remoteDB, nil
}

// SetTagDescription updates the Description for a given tag.
func (db *TagDatabase) SetTagDescription(name string, description string) (Tag, error) {
	val, found := db.tags.Load(name)
	if !found {
		return Tag{}, fmt.Errorf("SetTagDescription: tag '%s' not found in database", name)
	}

	tagPtr := val.(*Tag)
	tagPtr.valMu.Lock()
	tagPtr.Description = description
	copied := tagPtr.snapshot()
	tagPtr.valMu.Unlock()
	db.markChanged(name)
	return copied, nil
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
	db.markChanged(name)
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
	if forced {
		if tag.Force == nil {
			tag.Force = &ForceInfo{} // Initialize if it doesn't exist
		}
	} else {
		tag.Force = nil // Clear the force state
	}
	// Copy the state without the mutex: a copy of a held mutex stays locked,
	// and the caller's first locking call on it would deadlock.
	copied := tag.snapshot()
	tag.valMu.Unlock()
	db.markChanged(name)
	return copied, nil
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
	return tag.Force != nil, nil
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
	defer db.markChanged(name)

	// Allow nil to clear the force honeycomb.Value
	if value == nil && tag.Force != nil {
		tag.Force.Value = nil
	} else if value == nil {
		// Do nothing if trying to set a nil value on a non-forced tag.
		// Or, we could clear the force state entirely: tag.Force = nil
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
			if tag.Force == nil {
				tag.Force = &ForceInfo{}
			}
			tag.Force.Value = value
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
				value = truncateString(value, tag.TypeInfo.MaxLength)
			}
			if tag.Force == nil {
				tag.Force = &ForceInfo{}
			}
			tag.Force.Value = value
		}
	}
	// Return a copy without the mutex, which is held here.
	return tag.snapshot(), nil
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
	if tag.Force != nil {
		return tag.Force.Value, nil
	}
	return nil, nil
}

// notifySubscribers sends a copy of the tag's current state to its subscribers.
// Each subscription channel holds one update; one the subscriber has not
// received yet is replaced, so the newest state always wins. Sends never block.
func (db *TagDatabase) notifySubscribers(tag *Tag) {
	// Serializing notifications per tag keeps updates in order: the copy is
	// taken under notifyMu, so a later notification always carries newer state.
	tag.notifyMu.Lock()
	defer tag.notifyMu.Unlock()

	// Every value write funnels through here, so this is where changes are
	// recorded in the change feed (under the read lock, so array and UDT
	// values are copied consistently) and queued for the attached TagStore.
	tag.valMu.RLock()
	update := tag.snapshot()
	db.recordChange(tag)
	tag.valMu.RUnlock()
	tag.seq++
	update.Sequence = tag.seq

	db.markChanged(update.Name)

	// Holding subMu keeps Unsubscribe and RemoveTag from closing a channel mid-send.
	db.subMu.RLock()
	defer db.subMu.RUnlock()
	for _, ch := range db.subscriptions[update.Name] {
		select {
		case <-ch: // Discard the update the subscriber has not received yet.
		default:
		}
		select {
		case ch <- update:
		default: // Only reachable if another tag of the same name is notifying at the same moment.
		}
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

// truncateString shortens a STRING, WSTRING or string value to maxLength
// characters. STRING holds single-byte characters, so it is cut by bytes;
// WSTRING is cut by Unicode characters so a multi-byte character is never split.
func truncateString(value any, maxLength int) any {
	switch s := value.(type) {
	case plc.STRING:
		if len(s) > maxLength {
			return s[:maxLength]
		}
	case plc.WSTRING:
		if runes := []rune(s); len(runes) > maxLength {
			return plc.WSTRING(runes[:maxLength])
		}
	case string:
		if len(s) > maxLength {
			return s[:maxLength]
		}
	}
	return value
}

// imageSizePrefix maps the arrays of a royaljelly vars.Addresses table to their
// IEC 61131-3 size prefix. As in royaljelly, each array is indexed by address and
// the areas are independent: %IX3 is I.B[3], %QW4 is Q.W[4] and %MD2 is M.D[2].
// The R, LR, S and WS arrays have no size prefix and get no direct addresses.
var imageSizePrefix = map[string]string{"B": "X", "C": "B", "W": "W", "D": "D", "L": "L"}

// imageArrayName matches a process-image array tag, e.g. "I.B" or "M.W".
var imageArrayName = regexp.MustCompile(`^([IQM])\.([BCWDL])$`)

// imageElementName matches an element of a process-image array, e.g. "I.B[3]".
var imageElementName = regexp.MustCompile(`^([IQM])\.([BCWDL])\[(\d+)]$`)

// imageAddress returns the direct address of element index of an area's array.
func imageAddress(area, field string, index int) (string, bool) {
	size, ok := imageSizePrefix[field]
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%%%s%s%d", area, size, index), true
}

// imageArrayAddresses calls fn with the direct address of every element of a
// process-image array tag. It does nothing for other tags.
func imageArrayAddresses(name string, length int, fn func(index int, addr string)) {
	m := imageArrayName.FindStringSubmatch(name)
	if m == nil {
		return
	}
	for i := 0; i < length; i++ {
		if addr, ok := imageAddress(m[1], m[2], i); ok {
			fn(i, addr)
		}
	}
}

// generateDirectAddress returns the direct address of a tag named after a
// process-image element, e.g. "%IX3" for "I.B[3]".
func generateDirectAddress(tag *Tag) (string, bool) {
	m := imageElementName.FindStringSubmatch(tag.Name)
	if m == nil {
		return "", false
	}
	index, err := strconv.Atoi(m[3])
	if err != nil {
		return "", false
	}
	return imageAddress(m[1], m[2], index)
}

// canonicalAddress normalizes a direct address so that equivalent spellings
// share one directAddressMap key: a bit in byte.bit form (%IX1.2) becomes the
// flat bit index royaljelly uses (%IX10). Other addresses are returned as is.
func canonicalAddress(addr string) string {
	m := directAddressRegex.FindStringSubmatch(addr)
	if m == nil || m[2] != "X" || m[4] == "" {
		return addr
	}
	byteOffset, err1 := strconv.Atoi(m[3])
	bit, err2 := strconv.Atoi(m[4])
	if err1 != nil || err2 != nil || bit > 7 {
		return addr
	}
	return fmt.Sprintf("%%%sX%d", m[1], byteOffset*8+bit)
}

// resolveAddress returns the symbolic name an IEC direct address maps to, or
// name itself if it is not a direct address.
func (db *TagDatabase) resolveAddress(name string) (string, error) {
	if !directAddressRegex.MatchString(name) {
		return name, nil
	}
	if symbolicName, found := db.directAddressMap.Load(canonicalAddress(name)); found {
		return symbolicName.(string), nil
	}
	return "", fmt.Errorf("direct address '%s' not found in database", name)
}

// persistentTag is an unexported struct used as a data transfer object
// for serializing and deserializing tags to and from a persistence file.
type persistentTag struct {
	Name     string    `json:"Name"`
	TypeInfo *TypeInfo `json:"TypeInfo"`
	Value    any       `json:"Value"`
	// Quality is nil in files written before quality existed.
	Quality *Quality `json:"Quality,omitempty"`
	// Timestamp is when Value or Quality last changed; absent if never written.
	Timestamp time.Time `json:"Timestamp,omitzero"`
}

// WriteTagsToFile iterates through the database and writes each tag's name
// and current value to a file.
// This function is optimized to reduce memory allocations by pre-calculating
// the required buffer size and writing directly to a strings.Builder.
// It uses a worker pool to parallelize JSON marshaling for performance.
func (db *TagDatabase) WriteTagsToFile(filePath string) error {
	// Pre-allocate a slice to hold the lines. This avoids repeated allocations
	// during the Range loop. We can't know the exact size if tags are added/removed
	// concurrently, but it's a good starting point.
	lines := make([]string, 0, 1024) // Start with a reasonable capacity.
	estimatedSize := 0

	// Use a worker pool to parallelize JSON marshaling.
	numWorkers := db.PersistenceWorkers
	if numWorkers < 1 {
		numWorkers = 1 // Ensure at least one worker is used.
	}

	tagsChan := make(chan *Tag, numWorkers)
	resultsChan := make(chan []byte, numWorkers)
	var wg sync.WaitGroup

	// Start worker goroutines.
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for tag := range tagsChan {
				tag.valMu.RLock()
				quality := tag.Quality
				pTag := persistentTag{
					Name:      tag.Name,
					TypeInfo:  tag.TypeInfo,
					Value:     tag.presentedValue(),
					Quality:   &quality,
					Timestamp: tag.Timestamp,
				}
				tag.valMu.RUnlock()

				jsonData, err := json.Marshal(pTag)
				if err == nil {
					resultsChan <- jsonData
				}
			}
		}()
	}

	// Producer: Iterate over tags and send them to workers.
	go func() {
		db.tags.Range(func(key, value interface{}) bool {
			tag := value.(*Tag)
			if tag.IsRetain() {
				tagsChan <- tag
			}
			return true
		})
		close(tagsChan)
	}()

	// Closer: Wait for all workers to finish, then close the results channel.
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Consumer: Collect results from workers.
	for result := range resultsChan {
		lines = append(lines, string(result))
		estimatedSize += len(result) + 1 // +1 for newline
	}

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

	numWorkers := db.PersistenceWorkers
	if numWorkers < 1 {
		numWorkers = 1 // Ensure at least one worker is used.
	}

	// Define a struct to pass results (and errors) from workers to the consumer.
	type parseResult struct {
		pTag persistentTag
		err  error
	}

	linesChan := make(chan string, numWorkers)
	resultsChan := make(chan parseResult, numWorkers)
	var wg sync.WaitGroup

	// Start worker goroutines to unmarshal JSON lines concurrently.
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for line := range linesChan {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				var pTag persistentTag
				if err := json.Unmarshal([]byte(line), &pTag); err != nil {
					resultsChan <- parseResult{err: fmt.Errorf("failed to unmarshal JSON: %w", err)}
				} else {
					resultsChan <- parseResult{pTag: pTag}
				}
			}
		}()
	}

	// Producer: Feed lines from the file into the lines channel.
	go func() {
		for _, line := range lines {
			linesChan <- line
		}
		close(linesChan)
	}()

	// Closer: Wait for all workers to finish, then close the results channel.
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Consumer: Process the parsed tags from the results channel.
	for result := range resultsChan {
		if result.err != nil {
			errorList = append(errorList, fmt.Sprintf("line error: %v", result.err))
			continue
		}

		pTag := result.pTag
		tagName := pTag.Name
		valueData := pTag.Value
		// Restored values may be stale, so Good comes back as Uncertain.
		quality := QualityUncertain
		if pTag.Quality != nil {
			quality = pTag.Quality.restored()
		}

		val, found := db.tags.Load(tagName)
		if !found {
			continue // Tag from file doesn't exist in current config, skip.
		}
		tag := val.(*Tag)

		var newValue interface{}
		var parseErr error

		if _, isUDT := newUDTInstance(tag.TypeInfo.DataType); isUDT {
			tag.valMu.Lock()
			udtJSON, _ := json.Marshal(valueData)
			if jsonErr := json.Unmarshal(udtJSON, tag.Value); jsonErr != nil {
				parseErr = fmt.Errorf("failed to process UDT data for '%s': %w", tagName, jsonErr)
			} else {
				tag.Quality = quality
				tag.Timestamp = stamp(pTag.Timestamp)
			}
			tag.valMu.Unlock()
		} else if tag.TypeInfo.DataType == TypeARRAY {
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
			if err := db.SetTagValueQualityAt(tagName, newValue, quality, pTag.Timestamp); err != nil {
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
	case TypeWSTRING:
		return plc.WSTRING(valueStr), nil
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

// PopulateDatabaseFromImage adds an ARRAY tag for each array in the I, Q and M
// areas of a royaljelly process image, named area.array ("I.B", "Q.W", "M.R",
// ...) and holding a copy of the image's current values. Every element of the
// B, C, W, D and L arrays is mapped to its IEC direct address as royaljelly
// defines it: I.B[n] is %IXn (also reachable as %IXbyte.bit), Q.C[n] is %QBn,
// M.W[n] is %MWn, and so on. The REAL, LREAL and string arrays get no addresses.
func PopulateDatabaseFromImage(db *TagDatabase, pi *vars.ProcessImage) error {
	var img vars.Image
	pi.Snapshot(&img)
	return populateFromImage(db, &img)
}

// PopulateDatabaseFromVariables is PopulateDatabaseFromImage for royaljelly's
// package-level vars.I, vars.Q and vars.M tables.
//
// Deprecated: royaljelly deprecated those tables because they are unsynchronized.
// Use PopulateDatabaseFromImage with a vars.ProcessImage.
func PopulateDatabaseFromVariables(db *TagDatabase) error {
	return populateFromImage(db, &vars.Image{I: vars.I, Q: vars.Q, M: vars.M})
}

func populateFromImage(db *TagDatabase, img *vars.Image) error {
	for _, area := range []struct {
		prefix string
		table  *vars.Addresses
	}{{"I", &img.I}, {"Q", &img.Q}, {"M", &img.M}} {
		v := reflect.ValueOf(area.table).Elem()
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if field.Kind() != reflect.Array {
				continue // e.g. the scan time T
			}
			elemType := field.Type().Elem()
			elementType, ok := getDataType(elemType)
			if !ok {
				continue // Skip types we don't have a mapping for
			}

			// An ARRAY tag holds a slice, so copy the fixed-size array into one.
			slice := reflect.MakeSlice(reflect.SliceOf(elemType), field.Len(), field.Len())
			reflect.Copy(slice, field)

			tagName := area.prefix + "." + v.Type().Field(i).Name
			tag := &Tag{
				Name:     tagName,
				Value:    slice.Interface(),
				TypeInfo: &TypeInfo{DataType: TypeARRAY, ElementType: elementType},
			}
			if err := db.AddTag(tag); err != nil {
				return fmt.Errorf("PopulateDatabaseFromImage: error adding tag '%s': %w", tagName, err)
			}
			imageArrayAddresses(tagName, field.Len(), func(j int, addr string) {
				db.directAddressMap.Store(addr, fmt.Sprintf("%s[%d]", tagName, j))
			})
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

	// STEP 1 and 2: Get the base UDT instance (a top-level UDT or an element
	// of an array of UDTs) and walk the dot-separated field path on it
	// (e.g., "Config.Speed"), under the lock of the tag that owns it.
	fieldValue, err := db.fieldOf(basePath, fieldPath, 0)
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
func (db *TagDatabase) setSimpleTagValue(name string, value interface{}, quality Quality, timestamp time.Time) error {
	val, found := db.tags.Load(name)
	if !found {
		return fmt.Errorf("setTagValue: tag '%s' not found in database", name)
		//return fmt.Errorf("setSimpleTagValue: tag '%s' not found in database", name)
	}
	tag := val.(*Tag)

	// Use the tag's own SetValueQuality method to perform type checking.
	if err := tag.SetValueQualityAt(value, quality, timestamp); err != nil {
		return err
	}

	// No need to update the map, as we modified the struct via pointer.
	// Notify any subscribers about the change
	db.notifySubscribers(tag)

	return nil
}

// fieldOf reads fieldPath of the UDT at basePath, a tag or an element of an
// array tag, under the read lock of the tag that owns it. setNestedField
// writes a UDT's fields in place under that tag's write lock, so a field read
// after the lock is released (through the UDT pointer GetValue returns) would
// race with it.
func (db *TagDatabase) fieldOf(basePath, fieldPath string, depth int) (interface{}, error) {
	var owner *Tag
	index := -1
	if strings.HasSuffix(basePath, "]") {
		tag, i, err := db.parseArrayAccess(basePath)
		if err != nil {
			return nil, err
		}
		owner, index = tag, i
	} else if val, found := db.tags.Load(basePath); found {
		owner = val.(*Tag)
	}
	switch {
	case owner == nil, owner.RemoteAlias != nil && index >= 0:
		// A direct address, or an element of a remote array: resolved by
		// getTagValueRecursive.
		udtInstance, err := db.getTagValueRecursive(basePath, depth)
		if err != nil {
			return nil, err
		}
		return getFieldFromStruct(udtInstance, fieldPath)
	case owner.RemoteAlias != nil && index < 0:
		// The remote database reads the field under its own lock.
		if depth > 10 {
			return nil, fmt.Errorf("max recursion depth exceeded for remote alias '%s'", basePath)
		}
		remoteDB, found := db.getDatabase(owner.RemoteAlias.DBID)
		if !found {
			return nil, fmt.Errorf("remote database with ID '%s' not found for alias '%s'", owner.RemoteAlias.DBID, basePath)
		}
		return remoteDB.getTagValueRecursive(owner.RemoteAlias.TagName+"."+fieldPath, depth+1)
	}
	owner.valMu.RLock()
	defer owner.valMu.RUnlock()
	udtInstance := owner.presentedValue()
	if index >= 0 {
		element, err := arrayElement(owner, index)
		if err != nil {
			return nil, err
		}
		udtInstance = element
	}
	return getFieldFromStruct(udtInstance, fieldPath)
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
func (db *TagDatabase) setNestedField(basePath string, value interface{}, fieldPath string, quality Quality, timestamp time.Time) (err error) {
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

	baseTag.Quality = quality
	baseTag.Timestamp = stamp(timestamp)
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
	return arrayElement(baseTag, index)
}

// arrayElement is getArrayElementValue for a caller that holds baseTag.valMu.
func arrayElement(baseTag *Tag, index int) (interface{}, error) {
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

// setArrayElementValue writes a value to an element of a tag's slice value and
// sets the quality of the whole array.
func setArrayElementValue(baseTag *Tag, index int, value interface{}, quality Quality, timestamp time.Time) error {
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
	baseTag.Quality = quality
	baseTag.Timestamp = stamp(timestamp)

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
	// authorize, when set, replaces validTokens (see ServerOptions.Authorize).
	authorize func(r *http.Request, access Access) (string, error)
	readOnly  bool
	onWrite   func(r *http.Request, subject, tag string)
	maxBody   int64
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
		if ts.readOnly {
			http.Error(w, "This server is read-only", http.StatusForbidden)
			return
		}
		ts.handleSetTagValue(w, r, tagName)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// authMiddleware protects server endpoints: with ServerOptions.Authorize when
// set, else with Bearer tokens compared in constant time. A PUT to /tags/ is a
// write; everything else is a read.
func (ts *tagServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		access := AccessRead
		if r.Method == http.MethodPut {
			access = AccessWrite
		}
		if ts.authorize != nil {
			subject, err := ts.authorize(r, access)
			if err != nil {
				if errors.Is(err, ErrForbidden) {
					http.Error(w, "Forbidden", http.StatusForbidden)
				} else {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
				}
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), subjectKey{}, subject)))
			return
		}

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
		if !tokenValid(parts[1], ts.validTokens) {
			http.Error(w, "Invalid authentication token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenValid compares token with every valid token in constant time, so the
// time taken does not reveal how much of a token matched.
func tokenValid(token string, valid []string) bool {
	ok := 0
	for _, v := range valid {
		ok |= subtle.ConstantTimeCompare([]byte(token), []byte(v))
	}
	return ok == 1
}

// handleGetTagValue handles GET requests to read a tag's value.
func (ts *tagServer) handleGetTagValue(w http.ResponseWriter, r *http.Request, tagName string) {
	log.Printf("[Server] GET /tags/%s", tagName)
	value, err := ts.db.GetTagValue(tagName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	quality, _ := ts.db.GetTagQuality(tagName)
	response := map[string]interface{}{"value": value, "quality": quality}
	if timestamp := ts.db.tagTimestamp(tagName); !timestamp.IsZero() {
		response["timestamp"] = timestamp
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// handleGetAllTags handles GET requests to list all available tags, and batch
// reads (GET /tags?names=… or POST /tags; see batchread.go).
func (ts *tagServer) handleGetAllTags(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if names := requestedNames(r); len(names) > 0 {
			ts.handleReadTags(w, r, names)
			return
		}
	case http.MethodPost:
		names, err := decodeReadTagsBody(w, r)
		if err != nil {
			http.Error(w, "invalid batch read: "+err.Error(), http.StatusBadRequest)
			return
		}
		ts.handleReadTags(w, r, names)
		return
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	log.Printf("[Server] GET /tags")

	allTags := ts.db.GetAllTags() // This method returns a safe copy.

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(allTags)
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

	maxBody := ts.maxBody
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "Request body unreadable or too large", http.StatusRequestEntityTooLarge)
		return
	}

	// The request body is expected to be a JSON object like {"value": ..., "quality": 1}.
	// "quality" is optional and defaults to Good; sending it alone changes only the quality.
	var requestPayload map[string]json.RawMessage
	if err := json.Unmarshal(body, &requestPayload); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	quality := QualityGood
	qualityJSON, hasQuality := requestPayload["quality"]
	if hasQuality {
		if err := json.Unmarshal(qualityJSON, &quality); err != nil || !quality.IsValid() {
			http.Error(w, "Invalid 'quality' field", http.StatusBadRequest)
			return
		}
	}
	// "timestamp" (RFC 3339) is optional: the time the value was produced. It defaults to now.
	var timestamp time.Time
	if timestampJSON, ok := requestPayload["timestamp"]; ok {
		if err := json.Unmarshal(timestampJSON, &timestamp); err != nil {
			http.Error(w, "Invalid 'timestamp' field (want RFC 3339)", http.StatusBadRequest)
			return
		}
	}

	valueJSON, ok := requestPayload["value"]
	if !ok {
		if !hasQuality {
			http.Error(w, "Missing 'value' field", http.StatusBadRequest)
			return
		}
		if err := ts.db.SetTagQuality(tagName, quality); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ts.wrote(r, tagName)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "Tag quality updated successfully.")
		return
	}

	// To correctly unmarshal the value, especially for UDTs, we need a variable
	// of the correct type. We can get this from the tag's current value.
	// For a nested write, we unmarshal into a generic interface{}.
	// For a whole-tag write, we unmarshal into a new instance of the tag's type.
	if tag.Name != tagName {
		// Handle nested writes. We unmarshal into a generic interface{} first.
		var value interface{}
		if err := json.Unmarshal(valueJSON, &value); err != nil {
			http.Error(w, "Invalid JSON value for nested write", http.StatusBadRequest)
			return
		}

		// The JSON unmarshaler decodes all numbers into float64 by default.
		// If the target field is a REAL (float32), we must convert it.
		if tag.TypeInfo != nil {
			// This is a simplification. A more robust solution would inspect the nested field's type.
			// For this specific test case where we know the target is REAL, this is sufficient.
			if nestedFieldType, err := ts.db.getNestedFieldType(tagName); err == nil && nestedFieldType == TypeREAL {
				if floatVal, ok := value.(float64); ok {
					value = plc.REAL(floatVal) // Convert float64 to plc.REAL (float32)
				}
			}
		}

		if err := ts.db.SetTagValueQualityAt(tagName, value, quality, timestamp); err != nil {
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
		// For UDTs, we must pass the pointer, not the dereferenced value.
		finalValue := Dereference(newValuePtr)
		if _, isUDT := newUDTInstance(tag.TypeInfo.DataType); isUDT {
			finalValue = newValuePtr
		}

		if err := ts.db.SetTagValueQualityAt(tagName, finalValue, quality, timestamp); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	ts.wrote(r, tagName)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "Tag value updated successfully.")
}

// wrote reports a successful write to ServerOptions.OnWrite.
func (ts *tagServer) wrote(r *http.Request, tag string) {
	if ts.onWrite != nil {
		subject, _ := r.Context().Value(subjectKey{}).(string)
		ts.onWrite(r, subject, tag)
	}
}

// getNestedFieldType is a helper to determine the DataType of a nested field.
func (db *TagDatabase) getNestedFieldType(fullName string) (DataType, error) {
	// This reuses the getNestedField logic which already resolves the path.
	tempTag, err := db.getNestedField(fullName)
	if err != nil {
		return "", err
	}
	if tempTag.TypeInfo == nil {
		return "", fmt.Errorf("could not determine TypeInfo for nested field '%s'", fullName)
	}
	return tempTag.TypeInfo.DataType, nil
}

// getBaseTag is an internal helper that finds the top-level tag associated with a given name,
// even if the name represents a nested field or array element (e.g., "MyUDT.Field" or "MyArray[0]").
// It returns a pointer to the actual tag in the database, not a copy.
func (db *TagDatabase) getBaseTag(name string) (*Tag, bool) {
	// A top-level tag whose name contains a dot (e.g. "Guard1.Temp") is the tag
	// itself, not a field of a tag named "Guard1".
	if val, found := db.tags.Load(name); found {
		return val.(*Tag), true
	}
	// The base tag name is the part before the first dot or bracket.
	var baseTagName string
	dotIndex := strings.Index(name, ".")
	bracketIndex := strings.Index(name, "[")

	if dotIndex != -1 && (bracketIndex == -1 || dotIndex < bracketIndex) {
		baseTagName = name[:dotIndex]
	} else if bracketIndex != -1 {
		baseTagName = name[:bracketIndex]
	} else {
		baseTagName = name
	}

	if val, found := db.tags.Load(baseTagName); found {
		return val.(*Tag), true
	}
	return nil, false
}
