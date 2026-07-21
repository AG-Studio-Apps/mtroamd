//go:build !linux

package main

// findServeProcesses is Linux-only (/proc census); other platforms
// return nil and the doctor/update checks that consume it degrade to
// "no census available" rather than warning.
func findServeProcesses() []serveProcess { return nil }
