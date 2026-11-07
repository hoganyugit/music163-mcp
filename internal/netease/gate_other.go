//go:build !unix

package netease

func (g *Gate) waitShared() bool { return false }
