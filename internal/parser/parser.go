// Package parser defines the contract every parser plugs into the bot with.
// Each parser owns an independent poll loop (Run) and a set of chat commands
// (Handle) namespaced under its Name, e.g. /nike_start, /gr_check.
package parser

import "context"

type Parser interface {
	// Name is the command namespace and registry key, e.g. "nike" or "gr".
	Name() string

	// Run is the parser's independent poll loop; it returns when ctx is done.
	Run(ctx context.Context)

	// Handle processes "/<Name>_<action> <args>" for one chat and returns the
	// reply to send back. An empty reply means the action was unknown.
	Handle(chatID, action, args string) string

	// StatusLine is a short human-readable status for /status and
	// /<Name>_status, tailored to the requesting chat.
	StatusLine(chatID string) string
}
