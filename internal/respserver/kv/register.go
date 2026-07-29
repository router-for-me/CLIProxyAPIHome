package kv

import "github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"

// Register wires package handlers into the provided registry.
func Register(reg *dispatch.Registry) {
	if reg == nil {
		return
	}
	_ = reg.SetDirectDefault("SET", handleSet)
	_ = reg.SetDirectDefault("SETNX", handleSetNX)
	_ = reg.SetDirectDefault("CAS", handleCAS)
	_ = reg.SetDirectDefault("DEL", handleDel)
	_ = reg.SetDirectDefault("EXPIRE", handleExpire)
	_ = reg.SetDirectDefault("TTL", handleTTL)
	_ = reg.SetDirectDefault("INCRBY", handleIncrBy)
	_ = reg.SetDirectDefault("MGET", handleMGet)
	_ = reg.SetDirectDefault("MSET", handleMSet)
}
