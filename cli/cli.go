// Package cli supports the googet command-line interface.
package cli

import (
	"fmt"
	"strings"
)

// Confirmation returns true if the user affirms the prompt.
func Confirmation(msg string) bool {
	var c string
	fmt.Print(msg + " (y/N): ")
	fmt.Scanln(&c)
	c = strings.ToLower(c)
	return c == "y" || c == "yes"
}
