//go:build darwin

package main

import "syscall"

func init() {
	// This process and its test clients only; do not change the user's shell umask.
	syscall.Umask(0077)
}
