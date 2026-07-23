package push

import "github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"

// Register wires package handlers into the provided registry.
func Register(reg *dispatch.Registry) {
	if reg == nil {
		return
	}
	_ = reg.RegisterDirect("LPUSH", "usage", handleUsage)
	_ = reg.RegisterDirect("LPUSH", "in-flight-snapshot", handleInFlightSnapshot)
	_ = reg.RegisterDirect("LPUSH", "concurrency-release", handleConcurrencyRelease)
	_ = reg.SetDirectDefault("LPUSH", handleUsage)
	_ = reg.RegisterDirect("LPRUSH", "usage", handleUsage)

	_ = reg.RegisterDirect("RPUSH", "request-log", handleRequestLog)
	_ = reg.RegisterDirect("RPUSH", "app-log", handleAppLog)
	_ = reg.RegisterDirect("RPUSH", "plugin-status", handlePluginStatus)
}
