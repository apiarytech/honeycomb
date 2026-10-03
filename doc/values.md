# Reading and writing values

## Writes

| Call | Value | Quality | Timestamp |
|---|---|---|---|
| `SetTagValue(path, v)` | v | Good | now |
| `SetTagValueQuality(path, v, q)` | v | q | now |
| `SetTagValueQualityAt(path, v, q, t)` | v | q | t (now if zero) |
| `SetTagQuality(name, q)` | unchanged | q | now (only if q changes) |

- **Programs and HMIs** use `SetTagValue`: a value a program computes is
  Good.
- **Drivers** use `SetTagValueQualityAt` with the device's own timestamp,
  so a sequence-of-events record shows when the device saw the change, and
  `SetTagQuality(name, QualityBad)` when a read fails, which keeps the last
  value.

A write fails, and changes nothing, when the tag does not exist, is
`Constant`, or the value does not fit its `TypeInfo` (type, enumeration,
subrange). A field or element write (`Motor.Speed`, `Arr[1]`) changes the
quality and timestamp of the whole tag.

## Reads

| Call | Returns |
|---|---|
| `GetTagValue(path)` | the value (the force value if forced) |
| `GetTagQuality(path)` | the quality (of the whole tag for a field or element); `QualityBad` on error |
| `ReadTag(path)` | a `Reading{Value, Quality, Timestamp}` read together |
| `ReadTags(ctx, paths)` | a `Reading` for each path, in one call |

Use `ReadTag` when the three must belong together: reading value and
quality separately can mix two writes.

## Quality

| Quality | Value | Set when |
|---|---|---|
| `QualityUnknown` | 0 | A new tag that nothing has written |
| `QualityGood` | 1 | A plain write; a constant from the start |
| `QualityUncertain` | 2 | A Good value restored at power-up (it may be stale); a device that is busy or did not answer |
| `QualityBad` | 3 | A driver reports a failure; a read of a remote tag that fails |

Conversions to the industrial protocols, without their libraries:

| Protocol | To | From | Notes |
|---|---|---|---|
| OPC DA | `q.OPCDA() uint16` | `QualityFromOPCDA(uint16)` | Good `0xC0`, Uncertain `0x40`, Bad `0x00`; Unknown is `0x20` |
| OPC UA | `q.OPCUA() uint32` | `QualityFromOPCUA(uint32)` | By severity bits; Unknown is `BadWaitingForInitialData` |
| PLC4X | | `QualityFromPLC4X(code)` | `OK` Good; busy, pending and timeout Uncertain; the rest Bad |

## Timestamps

A tag's `Timestamp` is when its value or quality last changed: the time a
driver passed to `SetTagValueQualityAt`, or the time of the write. It is
zero until the first write, and it is kept by persistence and tag files.

## Batch reads

`ReadTags` reads many paths in one call. A path that cannot be read gets
`QualityBad` in its `Reading`, and the returned error joins every per-path
error, so one bad name does not hide the others:

```go
readings, err := db.ReadTags(ctx, []string{"Temp", "Lid", "Motors[1]"})
```

`TagDatabase` and `NetworkDatabaseClient` both implement `TagReader`, so the
same code reads in process and over the network (one HTTP request per
batch). An older server without batch reads returns
`ErrReadTagsUnsupported`.
