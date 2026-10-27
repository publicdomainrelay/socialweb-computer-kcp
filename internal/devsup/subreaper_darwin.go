//go:build darwin

package devsup

// ponytail: darwin has no child subreaper, so an orphaned grandchild reparents to launchd and is
// reaped by the env attribution sweep instead of the reaper; add a kqueue NOTE_EXIT watcher per
// unit if a restarted unit ever leaves a listener held past the sweep interval.
func becomeSubreaper() error { return nil }
