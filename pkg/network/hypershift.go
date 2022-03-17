package network

import "os"

var (
	enabled   = os.Getenv("HYPERSHIFT")
	namespace = os.Getenv("HOSTED_CLUSTER_NAMESPACE")
)

type HypershiftConfig struct {
	Enabled   bool
	Namespace string
}

func NewHypershiftConfig() HypershiftConfig {
	return HypershiftConfig{
		Enabled:   hypershiftEnabled(),
		Namespace: namespace,
	}
}

func hypershiftEnabled() bool {
	if enabled == "true" {
		return true
	}
	return false
}
