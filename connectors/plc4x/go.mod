module github.com/apiarytech/honeycomb/connectors/plc4x

go 1.27.3

require (
	github.com/apache/plc4x/plc4go v0.0.0-20261012073005-2b1095c45dbc
	github.com/apiarytech/honeycomb v0.4.0
	github.com/apiarytech/royaljelly v0.4.0
)

require (
	github.com/fatih/color v1.19.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.44.0 // indirect
)

// Develop against the honeycomb checkout in this repository.
replace github.com/apiarytech/honeycomb => ../..

// PLC4Go from ApiaryTech's fork (branch opcua-security), until the fixes are merged upstream:
// OPC UA chunking, security policies, browsing, working subscriptions and writes, and the
// codec and model fixes the OPC UA driver needs to talk to a server at all. A module using
// this one needs the same line.
replace github.com/apache/plc4x/plc4go => github.com/apiarytech/plc4x/plc4go v0.0.0-20261122123230-eb93b2cd9bd4
