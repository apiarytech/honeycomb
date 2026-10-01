# SQLite Persistence Example

Demonstrates the PLC persistence lifecycle with `store/sqlstore` and the pure-Go SQLite driver: restore retained tags at power-up, write changes behind the scan cycle, and save a final snapshot at shutdown.

```bash
cd examples/sqlite_persistence
go run .   # ScanCount starts at 0
go run .   # ScanCount continues from the previous run
```

Delete `plc.db*` to start over. See [store/README.md](../../store/README.md) for the architecture.
