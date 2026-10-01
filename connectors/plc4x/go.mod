module github.com/apiarytech/honeycomb/connectors/plc4x

go 1.27.1

require (
	github.com/apache/plc4x/plc4go v0.0.0-20260930074747-b878affa2d7b
	github.com/apiarytech/honeycomb v0.0.0
	github.com/apiarytech/royaljelly v0.1.0-beta1
)

require (
	github.com/fatih/color v1.19.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

// Develop against the honeycomb checkout in this repository.
replace github.com/apiarytech/honeycomb => ../..
