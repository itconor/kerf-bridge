//go:build !(darwin && cgo)

package main

// Only the Mac build is an app; everywhere else the bridge is a program or
// a service.

func inAppBundle() bool { return false }

func appLog() {}

func runApp(*Machine, *ConfigStore, int, func()) {}

func goQuit() {}
