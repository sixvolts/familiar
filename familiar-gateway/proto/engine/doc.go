// Package engine contains the generated protobuf message types for the
// in-process memory/identity engine (the former CoreEngine schema).
//
//go:generate protoc --go_out=. --go_opt=paths=source_relative -I. engine.proto
package engine
