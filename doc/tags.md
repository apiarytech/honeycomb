# Tags and types

## Data types

A tag's `TypeInfo.DataType` names its IEC 61131-3 type. The value is a Go
value of the matching royaljelly type (`import plc "github.com/apiarytech/royaljelly/iec"`):

| Kind | `DataType` constants | Go value |
|---|---|---|
| Boolean | `TypeBOOL` | `plc.BOOL` |
| Bit strings | `TypeBYTE`, `TypeWORD`, `TypeDWORD`, `TypeLWORD` | `plc.BYTE`, `plc.WORD`, ... |
| Signed integers | `TypeSINT`, `TypeINT`, `TypeDINT`, `TypeLINT` | `plc.SINT`, ... |
| Unsigned integers | `TypeUSINT`, `TypeUINT`, `TypeUDINT`, `TypeULINT` | `plc.USINT`, ... |
| Reals | `TypeREAL`, `TypeLREAL` | `plc.REAL` (float32), `plc.LREAL` (float64) |
| Complex | `TypeCOMPLEX`, `TypeLCOMPLEX` | royaljelly's complex types |
| Strings | `TypeSTRING`, `TypeWSTRING` | `plc.STRING`, `plc.WSTRING` |
| Time and date | `TypeTIME`, `TypeDATE`, `TypeTOD`, `TypeDT` | `plc.TIME`, `plc.DATE`, `plc.TOD`, `plc.DT` |
| Array | `TypeARRAY` (with `ElementType`) | a slice of the element type |
| Enumeration | a registered name, `TypeENUM` | `string` |
| Structure (UDT) | the UDT's `TypeName()` | a pointer to the struct |

Every write is checked against the type: writing an `plc.INT` to a `DINT`
tag fails. The Go type is the check, so no conversion happens silently.

## TypeInfo

```go
type TypeInfo struct {
    DataType    DataType    // BOOL, DINT, ARRAY, MotorData, ...
    ElementType DataType    // the element type of an ARRAY
    EnumValues  []string    // the values of an enumeration
    Min, Max    interface{} // a subrange, checked on every write
    MaxLength   int         // STRING/WSTRING: longer values are truncated; 0 is no limit
    Dimensions  []int       // the sizes of each dimension of an array, e.g. [2, 3]
}
```

`AddTag` infers the `TypeInfo` from the value when none is given (not for
UDTs with a nil value, which need a `TypeInfo`). Equal `TypeInfo`s are
shared through a registry, so a million `DINT` tags share one.

## Structures (UDTs)

A Go struct becomes an IEC STRUCT by implementing `UDT` and being
registered:

```go
type MotorData struct {
    Speed   plc.REAL
    Current plc.REAL
    Running plc.BOOL
}

func (*MotorData) TypeName() honeycomb.DataType { return "MotorData" }

honeycomb.RegisterUDT(&MotorData{})
db.AddTag(&honeycomb.Tag{Name: "Motor1", TypeInfo: &honeycomb.TypeInfo{DataType: "MotorData"}})
```

- A UDT tag added without a value gets a zero instance.
- Fields are read and written by path: `Motor1.Speed`, and nested
  `Drive.Config.MaxSpeed` when a field is itself a pointer to a UDT.
- Only exported fields are reachable.
- A field write changes the struct in place, under the tag's lock, and
  sets the **whole tag's** quality and timestamp.

## Enumerations

```go
honeycomb.RegisterENUM("MotorState", []string{"Stopped", "Running", "Faulted"})
db.AddTag(&honeycomb.Tag{Name: "State", TypeInfo: &honeycomb.TypeInfo{DataType: "MotorState"}, Value: "Stopped"})
db.SetTagValue("State", "Running") // a value not in the list fails
```

## Arrays

```go
db.AddTag(&honeycomb.Tag{
    Name:     "Levels",
    TypeInfo: &honeycomb.TypeInfo{DataType: honeycomb.TypeARRAY, ElementType: honeycomb.TypeREAL, Dimensions: []int{2, 3}},
    Value:    make([]plc.REAL, 6),
})
db.SetTagValue("Levels[1,2]", plc.REAL(4.5)) // row-major: element 1*3+2
db.GetTagValue("Levels[5]")                  // the same element by flat index
```

- An array is stored as a flat slice; multi-dimensional indexes are row
  major.
- Arrays of UDTs are addressed as `Motors[1].Speed`.
- An element write sets the whole array's quality and timestamp.

## Subranges and string lengths

`Min`/`Max` restrict numeric values: a write outside the range fails, for
values and for force values. `MaxLength` truncates longer strings instead
of failing.

## Names and access paths

Every read and write takes a **path**:

| Path | Meaning |
|---|---|
| `Tank1.Level` | the tag of that exact name (a name may contain dots) |
| `Motor1.Speed` | the field `Speed` of the UDT tag `Motor1`, when no tag is named `Motor1.Speed` |
| `Levels[3]`, `Levels[1,2]` | an element of an array tag |
| `Motors[1].Speed` | a field of an element of an array of UDTs |
| `%MW10`, `%IX1.2` | the tag with that direct address |

An exact tag name always wins over a field or element path, so
`Press1.Pressure` can be a tag of its own.

## Direct addresses

A tag can carry an IEC direct address (`DirectAddress: "%MW10"`): input
`%I`, output `%Q` or memory `%M`; size `X` (bit), `B`, `W`, `D` or `L`; an
offset, and for bits an optional `.bit`. Reads and writes accept the address
in place of the name. A bit has two spellings, `%IX1.2` and the flat bit
index `%IX10`; both find the same tag.

`PopulateDatabaseFromImage(db, image)` creates tags for a royaljelly
`vars.ProcessImage`, named after its areas (`Q.W[4]` for `%QW4`, `M.D[2]`
for `%MD2`) with the addresses filled in.

## Alias

`Alias` (set with `SetTagAlias`) is a second, descriptive name kept with the
tag and persisted. It is **metadata only**: reads and writes do not resolve
it. Use a remote alias or a direct address to reach a tag under another name.

## Remote aliases

A tag with `RemoteAlias: &RemoteAliasInfo{DBID, TagName}` has no value of
its own: reads and writes go to `TagName` in the database registered as
`DBID`. See [Network API](network.md#remote-aliases).

## Constants

A `Constant` tag's value cannot be written after `AddTag`, nor can it be
forced. Its configured value is Good from the start.

## Forcing

Forcing overrides what readers see without changing what writers write,
as on a PLC during commissioning:

```go
db.SetTagForced("Valve1", true)
db.SetTagForceValue("Valve1", plc.BOOL(true)) // type and subrange checked
v, _ := db.GetTagValue("Valve1")             // true, whatever the program writes
db.SetTagForced("Valve1", false)             // back to the written value
```

- While forced, writes still update `Value`; reads (`GetTagValue`,
  `ReadTag`, the network API, subscriptions) return the force value.
- Force state is persisted, and restored when
  `PersistOptions.RestoreForces` is set.
- The PLC4X connector writes a forced output tag's force value to the
  device.

## Managing tags

| Call | Does |
|---|---|
| `AddTag(*Tag)` | Adds a tag; fails if the name exists |
| `RemoveTag(name)` | Removes a tag |
| `RenameTag(old, new)` | Renames a tag, keeping its value and state |
| `GetTag(name)` | A lock-free copy of a tag (or of a field or element as a temporary tag) |
| `GetAllTags()`, `GetTags(names)`, `GetTagsByType(t)`, `GetAllTagNames()` | Copies or names |
| `SetTagDescription`, `GetTagDescription` | The description |

The `Tag` a call returns is a copy without locks; its `Value` may share
memory with the database (a UDT pointer, a slice), so do not modify it.
