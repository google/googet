// Package cli supports the googet command-line interface.
package cli

import (
	"context"
	"fmt"
	"strings"
)

// privilegedKey is the context key that marks a run as privileged.
type privilegedKey struct{}

// WithPrivileged returns a copy of ctx that marks the run as privileged. The
// googet binary sets it only after the admin check has passed and the lock is
// held, so subcommands can refuse work that needs those guarantees, and
// OpenDB and NewDownloader return resources that can write.
//
// The mark is not a security boundary inside the process: any code, including
// a test, can set it. It records what run() checked, so commands and the
// factories in this package fail closed when it is missing.
func WithPrivileged(ctx context.Context) context.Context {
	return context.WithValue(ctx, privilegedKey{}, true)
}

// IsPrivileged reports whether ctx was marked by WithPrivileged.
func IsPrivileged(ctx context.Context) bool {
	p, _ := ctx.Value(privilegedKey{}).(bool)
	return p
}

// Confirmation returns true if the user affirms the prompt.
func Confirmation(msg string) bool {
	var c string
	fmt.Print(msg + " (y/N): ")
	fmt.Scanln(&c)
	c = strings.ToLower(c)
	return c == "y" || c == "yes"
}
