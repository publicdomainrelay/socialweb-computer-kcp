//go:build !linux

package devcheck

func hostChecks(h Host) []Result {
	return []Result{notRoot(h)}
}
