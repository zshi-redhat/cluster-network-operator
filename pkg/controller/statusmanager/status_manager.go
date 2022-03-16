package statusmanager

import (
	"context"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/ghodss/yaml"

	configv1 "github.com/openshift/api/config/v1"
	operv1 "github.com/openshift/api/operator/v1"
	cnoclient "github.com/openshift/cluster-network-operator/pkg/client"
	"github.com/openshift/cluster-network-operator/pkg/names"
	"github.com/openshift/cluster-network-operator/pkg/network"
	hyperv1 "github.com/openshift/hypershift/api/v1alpha1"
	cohelpers "github.com/openshift/library-go/pkg/config/clusteroperator/v1helpers"
	operstatus "github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	uns "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type StatusLevel int

const (
	ClusterConfig StatusLevel = iota
	OperatorConfig
	ProxyConfig
	InjectorConfig
	PodDeployment
	PKIConfig
	EgressRouterConfig
	RolloutHung
	CertificateSigner
	maxStatusLevel
)

// StatusManager coordinates changes to ClusterOperator.Status
type StatusManager struct {
	sync.Mutex

	client cnoclient.Client
	name   string

	failing         [maxStatusLevel]*operv1.OperatorCondition
	installComplete bool

	daemonSets     []types.NamespacedName
	deployments    []types.NamespacedName
	statefulSets   []types.NamespacedName
	relatedObjects []configv1.ObjectReference

	hypershiftConfig network.HypershiftConfig
}

func New(client cnoclient.Client, name string) *StatusManager {
	return &StatusManager{
		client:           client,
		name:             name,
		hypershiftConfig: network.NewHypershiftConfig(),
	}
}

// setCOAnnotation sets an annotation on the clusterOperator network; or unsets it if value is nil
func (status *StatusManager) setCOAnnotation(obj *configv1.ClusterOperator, key string, value *string) error {
	new := obj.DeepCopy()
	anno := new.GetAnnotations()

	existing, set := anno[key]
	if value != nil && set && existing == *value {
		return nil
	}
	if !set && value == nil {
		return nil
	}

	if value != nil {
		if anno == nil {
			anno = map[string]string{}
		}
		anno[key] = *value
	} else {
		delete(anno, key)
	}
	new.SetAnnotations(anno)
	return status.client.Default().CRClient().Patch(context.TODO(), new, crclient.MergeFrom(obj))
}

// deleteRelatedObjects checks for related objects attached to ClusterOperator and deletes
// whatever is not been rendered from manifests. This is a mechanism to cleanup objects
// that are no longer needed and are probably present from a previous version
func (status *StatusManager) deleteRelatedObjectsNotRendered(co *configv1.ClusterOperator) {
	if status.relatedObjects == nil {
		return
	}
	for _, currentObj := range co.Status.RelatedObjects {
		var found bool = false
		for _, renderedObj := range status.relatedObjects {
			found = reflect.DeepEqual(currentObj, renderedObj)
			if found {
				break
			}
		}
		if !found {
			gvr := schema.GroupVersionResource{
				Group:    currentObj.Group,
				Resource: currentObj.Resource,
			}
			// TODO: delete resources in hypershift management cluster
			gvk, err := status.client.Default().RESTMapper().KindFor(gvr)
			if err != nil {
				log.Printf("Error getting GVK of object for deletion: %v", err)
				status.relatedObjects = append(status.relatedObjects, currentObj)
				continue
			}
			if gvk.Kind == "Namespace" && gvk.Group == "" {
				// BZ 1820472: During SDN migration, deleting a namespace object may get stuck in 'Terminating' forever if the cluster network doesn't working as expected.
				// We choose to not delete the namespace here but to ask user do it manually after the cluster is back to normal state.
				log.Printf("Object Kind is Namespace, skip")
				continue
			}
			// @aconstan: remove this after having the PR implementing this change, integrated.
			if gvk.Kind == "Network" && gvk.Group == "operator.openshift.io" {
				log.Printf("Object Kind is network.operator.openshift.io, skip")
				continue
			}
			log.Printf("Detected related object with GVK %+v, namespace %v and name %v not rendered by manifests, deleting...", gvk, currentObj.Namespace, currentObj.Name)
			objToDelete := &uns.Unstructured{}
			objToDelete.SetName(currentObj.Name)
			objToDelete.SetNamespace(currentObj.Namespace)
			objToDelete.SetGroupVersionKind(gvk)
			err = status.client.Default().CRClient().Delete(context.TODO(), objToDelete, crclient.PropagationPolicy("Background"))
			if err != nil {
				log.Printf("Error deleting related object: %v", err)
				if !errors.IsNotFound(err) {
					status.relatedObjects = append(status.relatedObjects, currentObj)
				}
				continue
			}
		}
	}
}

// Set updates the operator and clusteroperator statuses with the provided conditions.
func (status *StatusManager) set(reachedAvailableLevel bool, conditions ...operv1.OperatorCondition) {
	var operStatus *operv1.NetworkStatus

	// Set status on the network.operator object
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		oc := &operv1.Network{ObjectMeta: metav1.ObjectMeta{Name: names.OPERATOR_CONFIG}}
		err := status.client.Default().CRClient().Get(context.TODO(), types.NamespacedName{Name: names.OPERATOR_CONFIG}, oc)
		if err != nil {
			// Should never happen outside of unit tests
			return err
		}

		oldStatus := oc.Status.DeepCopy()

		oc.Status.Version = os.Getenv("RELEASE_VERSION")

		// If there is no Available condition on the operator config then copy the
		// conditions from the ClusterOperator (which will either also be empty if
		// this is a new install, or will contain the pre-4.7 conditions if this is
		// a 4.6->4.7 upgrade).
		if v1helpers.FindOperatorCondition(oc.Status.Conditions, operv1.OperatorStatusTypeAvailable) == nil {
			co := &configv1.ClusterOperator{ObjectMeta: metav1.ObjectMeta{Name: status.name}}
			err := status.client.Default().CRClient().Get(context.TODO(), types.NamespacedName{Name: status.name}, co)
			if err != nil {
				log.Printf("failed to retrieve ClusterOperator object: %v - continuing", err)
			}

			for _, condition := range co.Status.Conditions {
				v1helpers.SetOperatorCondition(&oc.Status.Conditions, operv1.OperatorCondition{
					Type:               string(condition.Type),
					Status:             operv1.ConditionStatus(condition.Status),
					LastTransitionTime: condition.LastTransitionTime,
					Reason:             condition.Reason,
					Message:            condition.Message,
				})
			}
		}

		for _, condition := range conditions {
			v1helpers.SetOperatorCondition(&oc.Status.Conditions, condition)
		}

		progressingCondition := v1helpers.FindOperatorCondition(oc.Status.Conditions, operv1.OperatorStatusTypeProgressing)
		availableCondition := v1helpers.FindOperatorCondition(oc.Status.Conditions, operv1.OperatorStatusTypeAvailable)
		if availableCondition == nil && progressingCondition != nil && progressingCondition.Status == operv1.ConditionTrue {
			v1helpers.SetOperatorCondition(&oc.Status.Conditions,
				operv1.OperatorCondition{
					Type:    operv1.OperatorStatusTypeAvailable,
					Status:  operv1.ConditionFalse,
					Reason:  "Startup",
					Message: "The network is starting up",
				},
			)
		}

		v1helpers.SetOperatorCondition(&oc.Status.Conditions,
			operv1.OperatorCondition{
				Type:   operv1.OperatorStatusTypeUpgradeable,
				Status: operv1.ConditionTrue,
			},
		)

		operStatus = &oc.Status

		if equality.Semantic.DeepEqual(&oc.Status, oldStatus) {
			return nil
		}

		buf, err := yaml.Marshal(oc.Status.Conditions)
		if err != nil {
			buf = []byte(fmt.Sprintf("(failed to convert to YAML: %s)", err))
		}

		if err := status.client.Default().CRClient().Update(context.TODO(), oc); err != nil {
			return err
		}
		log.Printf("Set operator conditions:\n%s", buf)

		return nil
	})
	if err != nil {
		log.Printf("Failed to set operator status: %v", err)
	}

	if status.hypershiftConfig.Enabled {
		err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: status.hypershiftConfig.Name}}
			gvr := schema.GroupVersionResource{
				Group:    "hypershift.openshift.io",
				Version:  "v1alpha1",
				Resource: "hostedcontrolplanes",
			}
			hcpObj, err := status.client.ClientFor(cnoclient.ManagementClusterName).Dynamic().Resource(gvr).Namespace(
				status.hypershiftConfig.Namespace).Get(context.TODO(), status.hypershiftConfig.Name, metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					log.Printf("Did not find hostedControlPlane")
				} else {
					return err
				}
			} else {
				err := runtime.DefaultUnstructuredConverter.FromUnstructured(hcpObj.UnstructuredContent(), hcp)
				if err != nil {
					return err
				}
			}

			oldStatus := hcp.Status.DeepCopy()
			if operStatus == nil {
				meta.SetStatusCondition(&hcp.Status.Conditions, metav1.Condition{
					Type:    "NetworkOperatorAvailable",
					Status:  metav1.ConditionUnknown,
					Reason:  "NoNetworkOperConfig",
					Message: "No networks.operator.openshift.io cluster found",
				})
			} else {
				for _, cond := range operStatus.Conditions {
					if cond.Type == operv1.OperatorStatusTypeProgressing {
						newCondition := metav1.Condition{
							Type:    network.NetworkOperatorStatusTypeProgressing,
							Status:  metav1.ConditionStatus(cond.Status),
							Reason:  cond.Reason,
							Message: cond.Message,
						}
						meta.SetStatusCondition(&hcp.Status.Conditions, newCondition)
					}
					if cond.Type == operv1.OperatorStatusTypeDegraded {
						newCondition := metav1.Condition{
							Type:    network.NetworkOperatorStatusTypeDegraded,
							Status:  metav1.ConditionStatus(cond.Status),
							Reason:  cond.Reason,
							Message: cond.Message,
						}
						meta.SetStatusCondition(&hcp.Status.Conditions, newCondition)
					}
				}
			}

			if reflect.DeepEqual(*oldStatus, hcp.Status) {
				return nil
			}

			buf, err := yaml.Marshal(hcp.Status.Conditions)
			if err != nil {
				buf = []byte(fmt.Sprintf("(failed to convert to YAML: %s)", err))
			}

			if err := status.client.ClientFor(cnoclient.ManagementClusterName).CRClient().Status().Update(context.TODO(), hcp); err != nil {
				return err
			}
			log.Printf("Set HostedControlPlane conditions:\n%s", buf)
			return nil
		})
		if err != nil {
			log.Printf("Failed to set HostedControlPlane: %v", err)
		}
	}

	// Set status conditions on the network clusteroperator object.
	// TODO: enable the library-go ClusterOperatorStatusController, which will
	// do this for us. We can't use that yet, because it doesn't allow dynamic RelatedObjects[].
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		co := &configv1.ClusterOperator{ObjectMeta: metav1.ObjectMeta{Name: status.name}}
		err := status.client.Default().CRClient().Get(context.TODO(), types.NamespacedName{Name: status.name}, co)
		isNotFound := errors.IsNotFound(err)
		if err != nil && !isNotFound {
			return err
		}

		oldStatus := co.Status.DeepCopy()
		status.deleteRelatedObjectsNotRendered(co)
		if status.relatedObjects != nil {
			co.Status.RelatedObjects = status.relatedObjects
		}

		if status.hypershiftConfig.RelatedObjects != nil {
			objs := []string{}
			for _, obj := range status.hypershiftConfig.RelatedObjects {
				objs = append(objs, fmt.Sprintf("%s/%s/%s", obj.ClusterName, obj.Reference.Namespace, obj.Reference.Name))
			}
			annotValue := strings.Join(objs, ",")
			err := status.setCOAnnotation(co, "network.operator.openshift.io/relatedObjects", &annotValue)
			if err != nil {
				return err
			}
		}

		if operStatus == nil {
			cohelpers.SetStatusCondition(&co.Status.Conditions, configv1.ClusterOperatorStatusCondition{
				Type:    configv1.OperatorDegraded,
				Status:  configv1.ConditionTrue,
				Reason:  "NoOperConfig",
				Message: "No networks.operator.openshift.io cluster found",
			})
		} else {
			if reachedAvailableLevel {
				co.Status.Versions = []configv1.OperandVersion{
					{Name: "operator", Version: operStatus.Version},
				}
			}

			for _, cond := range operStatus.Conditions {
				cohelpers.SetStatusCondition(&co.Status.Conditions, operstatus.OperatorConditionToClusterOperatorCondition(cond))
			}
		}

		if reflect.DeepEqual(*oldStatus, co.Status) {
			return nil
		}

		buf, err := yaml.Marshal(co.Status.Conditions)
		if err != nil {
			buf = []byte(fmt.Sprintf("(failed to convert to YAML: %s)", err))
		}

		if isNotFound {
			if err := status.client.Default().CRClient().Create(context.TODO(), co); err != nil {
				return err
			}
			log.Printf("Set ClusterOperator conditions:\n%s", buf)
			return nil
		}
		if err := status.client.Default().CRClient().Status().Update(context.TODO(), co); err != nil {
			return err
		}
		log.Printf("Set ClusterOperator conditions:\n%s", buf)
		return nil
	})
	if err != nil {
		log.Printf("Failed to set ClusterOperator: %v", err)
	}
}

// syncDegraded syncs the current Degraded status
func (status *StatusManager) syncDegraded() {
	for _, c := range status.failing {
		if c != nil {
			status.set(false, *c)
			return
		}
	}
	status.set(
		false,
		operv1.OperatorCondition{
			Type:   operv1.OperatorStatusTypeDegraded,
			Status: operv1.ConditionFalse,
		},
	)
}

func (status *StatusManager) setDegraded(statusLevel StatusLevel, reason, message string) {
	status.failing[statusLevel] = &operv1.OperatorCondition{
		Type:    operv1.OperatorStatusTypeDegraded,
		Status:  operv1.ConditionTrue,
		Reason:  reason,
		Message: message,
	}
	status.syncDegraded()
}

func (status *StatusManager) setNotDegraded(statusLevel StatusLevel) {
	if status.failing[statusLevel] != nil {
		status.failing[statusLevel] = nil
	}
	status.syncDegraded()
}

func (status *StatusManager) SetDegraded(statusLevel StatusLevel, reason, message string) {
	status.Lock()
	defer status.Unlock()
	status.setDegraded(statusLevel, reason, message)
}

func (status *StatusManager) SetNotDegraded(statusLevel StatusLevel) {
	status.Lock()
	defer status.Unlock()
	status.setNotDegraded(statusLevel)
}

func (status *StatusManager) SetDaemonSets(daemonSets []types.NamespacedName) {
	status.Lock()
	defer status.Unlock()
	status.daemonSets = daemonSets
}

func (status *StatusManager) SetDeployments(deployments []types.NamespacedName) {
	status.Lock()
	defer status.Unlock()
	status.deployments = deployments
}

func (status *StatusManager) SetStatefulSets(statefulSets []types.NamespacedName) {
	status.Lock()
	defer status.Unlock()
	status.statefulSets = statefulSets
}

func (status *StatusManager) SetRelatedObjects(relatedObjects []configv1.ObjectReference) {
	status.Lock()
	defer status.Unlock()
	status.relatedObjects = relatedObjects
}
