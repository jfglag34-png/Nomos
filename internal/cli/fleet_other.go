//go:build !linux

package cli

import "fmt"

func RunFleet(args []string) { fmt.Println("wedra fleet requires Linux (Ubuntu Server)") }
