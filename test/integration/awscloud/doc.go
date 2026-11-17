// Package awscloud runs the operator's ElastiCache and MemoryDB backends against
// moto server and the fake in front of it (see README.md in the parent
// directory). Its tests have the build tags integration and moto, so neither
// `go test ./...` nor the Valkey job of the Integration workflow compiles them.
package awscloud
