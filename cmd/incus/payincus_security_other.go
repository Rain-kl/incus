//go:build !linux

package main

// Non-Linux clients have no local node security mode; native server authorization still applies.
func observePayIncusCLI(_ []string) error { return nil }
