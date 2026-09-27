//go:build !linux

package collect

// Disk is Linux's. Elsewhere (a developer's machine running the agent
// against fixtures) there is no disk to report, and the platform reads a
// total of 0 as "unknown".
func Disk(path string) (used, total uint64, err error) {
	return 0, 0, nil
}
