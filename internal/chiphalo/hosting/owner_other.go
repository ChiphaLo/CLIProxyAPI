//go:build !linux

package hosting

func deploymentOwned(string) bool   { return false }
func deploymentParents(string) bool { return false }
