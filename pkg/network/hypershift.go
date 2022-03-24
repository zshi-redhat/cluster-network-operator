package network

import (
	"os"
	"sync"

	configv1 "github.com/openshift/api/config/v1"
)

var (
	enabled   = os.Getenv("HYPERSHIFT")
	name      = os.Getenv("HOSTED_CLUSTER_NAME")
	namespace = os.Getenv("HOSTED_CLUSTER_NAMESPACE")

	// NetworkOperatorStatusTypeProgressing indicates Degraded condition in hostedControlPlane status
	NetworkOperatorStatusTypeProgressing = "network.operator.openshift.io/Progressing"
	// NetworkOperatorStatusTypeDegraded indicates Degraded condition in hostedControlPlane status
	NetworkOperatorStatusTypeDegraded = "network.operator.openshift.io/Degraded"
)

type RelatedObject struct {
	configv1.ObjectReference
	ClusterName string
}

type HypershiftConfig struct {
	sync.Mutex
	Enabled        bool
	Name           string
	Namespace      string
	RelatedObjects []RelatedObject
}

func NewHypershiftConfig() HypershiftConfig {
	return HypershiftConfig{
		Enabled:   hypershiftEnabled(),
		Name:      name,
		Namespace: namespace,
	}
}

func hypershiftEnabled() bool {
	return enabled == "true"
}

func (hc *HypershiftConfig) SetRelatedObjects(relatedObjects []RelatedObject) {
	hc.Lock()
	defer hc.Unlock()
	hc.RelatedObjects = relatedObjects
}
