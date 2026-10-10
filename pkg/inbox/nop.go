// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox

import "context"

// NopHooks is a Hooks that does nothing, the open-source default. Embed it in
// your own type to override only the hook you need.
type NopHooks struct{}

// BeforeIngest implements Hooks and never rejects.
func (NopHooks) BeforeIngest(context.Context, *Inbox, int64) error { return nil }

// AfterIngest implements Hooks and does nothing.
func (NopHooks) AfterIngest(context.Context, *Inbox, *Message, bool) {}

// NopNotifier is a Notifier that never signals: Publish discards, and a
// subscription never fires. It is what a server uses until a real notifier is
// configured; subscribers then rely on periodic re-reads.
type NopNotifier struct{}

// Publish implements Notifier and does nothing.
func (NopNotifier) Publish(context.Context, ID, int64) {}

// Subscribe implements Notifier. The returned channel never receives.
func (NopNotifier) Subscribe(context.Context, ID) (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}

var (
	_ Hooks    = NopHooks{}
	_ Notifier = NopNotifier{}
)
