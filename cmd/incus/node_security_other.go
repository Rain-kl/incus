//go:build !linux

package main

// Non-Linux clients have no local node security mode; native server authorization still applies.
func observeIncusCLI(_ []string) error { return nil }
