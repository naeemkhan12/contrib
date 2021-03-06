/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"
	goflag "flag"
	"fmt"
	"net/http"
	"os"
	"time"

	ca_simulator "k8s.io/autoscaler/cluster-autoscaler/simulator"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	kube_utils "k8s.io/autoscaler/cluster-autoscaler/utils/kubernetes"
	kube_client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	kube_restclient "k8s.io/client-go/rest"
	kube_record "k8s.io/client-go/tools/record"
	"k8s.io/contrib/rescheduler/metrics"
	kubeapi "k8s.io/kubernetes/pkg/apis/core"
	kubectl_util "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/scheduler/schedulercache"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	flag "github.com/spf13/pflag"
)

const (
	criticalPodAnnotation      = "scheduler.alpha.kubernetes.io/critical-pod"
	criticalAddonsOnlyTaintKey = "CriticalAddonsOnly"
	// TaintsAnnotationKey represents the key of taints data (json serialized)
	// in the Annotations of a Node.
	TaintsAnnotationKey string = "scheduler.alpha.kubernetes.io/taints"

	// HighestUserDefinablePriority is the highest priority for user defined priority classes. Priority values larger than 1 billion are reserved for Kubernetes system use.
	HighestUserDefinablePriority = int32(1000000000)
	// SystemCriticalPriority is the beginning of the range of priority values for critical system components.
	SystemCriticalPriority = 2 * HighestUserDefinablePriority
)

var (
	flags = flag.NewFlagSet(
		`rescheduler: rescheduler --running-in-cluster=true`,
		flag.ExitOnError)

	inCluster = flags.Bool("running-in-cluster", true,
		`Optional, if this controller is running in a kubernetes cluster, use the
		 pod secrets for creating a Kubernetes client.`)

	contentType = flags.String("kube-api-content-type", "application/vnd.kubernetes.protobuf",
		`Content type of requests sent to apiserver.`)

	housekeepingInterval = flags.Duration("housekeeping-interval", 10*time.Second,
		`How often rescheduler takes actions.`)

	systemNamespace = flags.String("system-namespace", metav1.NamespaceSystem,
		`Namespace to watch for critical addons.`)

	initialDelay = flags.Duration("initial-delay", 2*time.Minute,
		`How long should rescheduler wait after start to make sure
		 all critical addons had a chance to start.`)

	podScheduledTimeout = flags.Duration("pod-scheduled-timeout", 10*time.Minute,
		`How long should rescheduler wait for critical pod to be scheduled
		 after evicting pods to make a spot for it.`)

	listenAddress = flags.String("listen-address", "127.0.0.1:9235",
		`Address to listen on for serving prometheus metrics`)

	gracePeriod = flags.Duration("grace-period", 10*time.Second,
		"How long to wait for rescheduled pods to terminate. If negative, the grace period specified in each pod"+
			" will be used. If 0, pods will be immediately terminated.")
)

func main() {
	flags.AddGoFlagSet(goflag.CommandLine)

	// Log to stderr by default and fix usage message accordingly
	logToStdErr := flags.Lookup("logtostderr")
	logToStdErr.DefValue = "true"
	flags.Set("logtostderr", "true")

	flags.Parse(os.Args)

	glog.Infof("Running Rescheduler")

	go func() {
		http.Handle("/metrics", prometheus.Handler())
		err := http.ListenAndServe(*listenAddress, nil)
		glog.Fatalf("Failed to start metrics: %v", err)
	}()

	// TODO(piosz): figure out a better way of verifying cluster stabilization here.
	time.Sleep(*initialDelay)

	kubeClient, err := createKubeClient(flags, *inCluster)
	if err != nil {
		glog.Fatalf("Failed to create kube client: %v", err)
	}

	recorder := createEventRecorder(kubeClient)
	predicateCheckerStopChannel := make(chan struct{})
	predicateChecker, err := ca_simulator.NewPredicateChecker(kubeClient, predicateCheckerStopChannel)
	if err != nil {
		glog.Fatalf("Failed to create predicate checker: %v", err)
	}

	stopChannel := make(chan struct{})
	unschedulablePodLister := kube_utils.NewUnschedulablePodInNamespaceLister(kubeClient, *systemNamespace, stopChannel)
	nodeLister := kube_utils.NewReadyNodeLister(kubeClient, stopChannel)

	// TODO(piosz): consider reseting this set once every few hours.
	podsBeingProcessed := NewPodSet()

	// As tolerations/taints feature changed from being specified in annotations
	// to being specified in fields in Kubernetes 1.6, we need to make sure that
	// any annotations that were created in the previous versions are removed.
	releaseAllTaintsDeprecated(kubeClient, nodeLister)

	releaseAllTaints(kubeClient, nodeLister, podsBeingProcessed)

	for {
		select {
		case <-time.After(*housekeepingInterval):
			{
				allUnschedulablePods, err := unschedulablePodLister.List()
				if err != nil {
					glog.Errorf("Failed to list unscheduled pods: %v", err)
					continue
				}

				criticalDaemonSetPods := filterCriticalDaemonSetPods(allUnschedulablePods, podsBeingProcessed)

				if len(criticalDaemonSetPods) > 0 {
					for _, pod := range criticalDaemonSetPods {
						glog.Infof("Critical pod %s is unschedulable. Trying to find a spot for it.", podId(pod))
						k8sApp := "unknown"
						if l, found := pod.ObjectMeta.Labels["k8s-app"]; found {
							k8sApp = l
						}
						metrics.UnschedulableCriticalPodsCount.WithLabelValues(k8sApp).Inc()
						nodes, err := nodeLister.List()
						if err != nil {
							glog.Errorf("Failed to list nodes: %v", err)
							continue
						}

						node := findNodeForPod(kubeClient, predicateChecker, nodes, pod)
						if node == nil {
							glog.Errorf("Pod %s can't be scheduled on any existing node.", podId(pod))
							recorder.Eventf(pod, v1.EventTypeNormal, "PodDoestFitAnyNode",
								"Critical pod %s doesn't fit on any node.", podId(pod))
							continue
						}
						glog.Infof("Trying to place the pod on node %v", node.Name)

						err = prepareNodeForPod(kubeClient, recorder, predicateChecker, node, pod)
						if err != nil {
							glog.Warningf("%+v", err)
						} else {
							podsBeingProcessed.Add(pod)
							go waitForScheduled(kubeClient, podsBeingProcessed, pod)
						}
					}
				}

				releaseAllTaints(kubeClient, nodeLister, podsBeingProcessed)
			}
		}
	}
}

func waitForScheduled(client kube_client.Interface, podsBeingProcessed *podSet, pod *v1.Pod) {
	glog.Infof("Waiting for pod %s to be scheduled", podId(pod))
	err := wait.Poll(time.Second, *podScheduledTimeout, func() (bool, error) {
		p, err := client.CoreV1().Pods(pod.Namespace).Get(pod.Name, metav1.GetOptions{})
		if err != nil {
			glog.Warningf("Error while getting pod %s: %v", podId(pod), err)
			return false, nil
		}
		return p.Spec.NodeName != "", nil
	})
	if err != nil {
		glog.Warningf("Timeout while waiting for pod %s to be scheduled after %v.", podId(pod), *podScheduledTimeout)
	} else {
		glog.Infof("Pod %v was successfully scheduled.", podId(pod))
	}
	podsBeingProcessed.Remove(pod)
}

func createKubeClient(flags *flag.FlagSet, inCluster bool) (kube_client.Interface, error) {
	var config *kube_restclient.Config
	var err error
	if inCluster {
		config, err = kube_restclient.InClusterConfig()
	} else {
		clientConfig := kubectl_util.DefaultClientConfig(flags)
		config, err = clientConfig.ClientConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("error connecting to the client: %v", err)
	}
	config.ContentType = *contentType
	return kube_client.NewForConfigOrDie(config), nil
}

func createEventRecorder(client kube_client.Interface) kube_record.EventRecorder {
	eventBroadcaster := kube_record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(client.CoreV1().RESTClient()).Events("")})
	return eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "rescheduler"})
}

// copied from Kubernetes 1.5.4
func getTaintsFromNodeAnnotations(annotations map[string]string) ([]v1.Taint, error) {
	var taints []v1.Taint
	if len(annotations) > 0 && annotations[TaintsAnnotationKey] != "" {
		err := json.Unmarshal([]byte(annotations[TaintsAnnotationKey]), &taints)
		if err != nil {
			return []v1.Taint{}, err
		}
	}
	return taints, nil
}

func releaseAllTaintsDeprecated(client kube_client.Interface, nodeLister kube_utils.NodeLister) {
	glog.Infof("Removing all annotation taints because they are no longer supported.")
	nodes, err := nodeLister.List()
	if err != nil {
		glog.Warningf("Cannot release taints - error while listing nodes: %v", err)
		return
	}
	releaseTaintsOnNodesDeprecated(client, nodes)
}

func releaseTaintsOnNodesDeprecated(client kube_client.Interface, nodes []*v1.Node) {
	for _, node := range nodes {
		taints, err := getTaintsFromNodeAnnotations(node.Annotations)
		if err != nil {
			glog.Warningf("Error while getting Taints for node %v: %v", node.Name, err)
			continue
		}

		newTaints := make([]v1.Taint, 0)
		for _, taint := range taints {
			if taint.Key == criticalAddonsOnlyTaintKey {
				glog.Infof("Releasing taint %+v on node %v", taint, node.Name)
			} else {
				newTaints = append(newTaints, taint)
			}
		}

		if len(newTaints) != len(taints) {
			taintsJson, err := json.Marshal(newTaints)
			if err != nil {
				glog.Warningf("Error while releasing taints on node %v: %v", node.Name, err)
				continue
			}

			node.Annotations[TaintsAnnotationKey] = string(taintsJson)
			_, err = client.CoreV1().Nodes().Update(node)
			if err != nil {
				glog.Warningf("Error while releasing taints on node %v: %v", node.Name, err)
			} else {
				glog.Infof("Successfully released all taints on node %v", node.Name)
			}
		}
	}
}

func releaseAllTaints(client kube_client.Interface, nodeLister kube_utils.NodeLister, podsBeingProcessed *podSet) {
	nodes, err := nodeLister.List()
	if err != nil {
		glog.Warningf("Cannot release taints - error while listing nodes: %v", err)
		return
	}
	releaseTaintsOnNodes(client, nodes, podsBeingProcessed)
}

func releaseTaintsOnNodes(client kube_client.Interface, nodes []*v1.Node, podsBeingProcessed *podSet) {
	for _, node := range nodes {
		newTaints := make([]v1.Taint, 0)
		for _, taint := range node.Spec.Taints {
			if taint.Key == criticalAddonsOnlyTaintKey && !podsBeingProcessed.HasId(taint.Value) {
				glog.Infof("Releasing taint %+v on node %v", taint, node.Name)
			} else {
				newTaints = append(newTaints, taint)
			}
		}

		if len(newTaints) != len(node.Spec.Taints) {
			node.Spec.Taints = newTaints
			_, err := client.CoreV1().Nodes().Update(node)
			if err != nil {
				glog.Warningf("Error while releasing taints on node %v: %v", node.Name, err)
			} else {
				glog.Infof("Successfully released all taints on node %v", node.Name)
			}
		}
	}
}

// The caller of this function must remove the taint if this function returns error.
func prepareNodeForPod(client kube_client.Interface, recorder kube_record.EventRecorder, predicateChecker *ca_simulator.PredicateChecker, originalNode *v1.Node, criticalPod *v1.Pod) error {
	// Operate on a copy of the node to ensure pods running on the node will pass CheckPredicates below.
	node := originalNode.DeepCopy()
	err := addTaint(client, originalNode, podId(criticalPod))
	if err != nil {
		return fmt.Errorf("Error while adding taint: %v", err)
	}

	requiredPods, otherPods, err := groupPods(client, node)
	if err != nil {
		return err
	}

	nodeInfo := schedulercache.NewNodeInfo(requiredPods...)
	nodeInfo.SetNode(node)

	// check whether critical pod still fit
	if err := predicateChecker.CheckPredicates(criticalPod, nil, nodeInfo, true); err != nil {
		return fmt.Errorf("Pod %s doesn't fit to node %v: %v", podId(criticalPod), node.Name, err)
	}
	requiredPods = append(requiredPods, criticalPod)
	nodeInfo = schedulercache.NewNodeInfo(requiredPods...)
	nodeInfo.SetNode(node)

	for _, p := range otherPods {
		if err := predicateChecker.CheckPredicates(p, nil, nodeInfo, true); err != nil {
			glog.Infof("Pod %s will be deleted in order to schedule critical pod %s.", podId(p), podId(criticalPod))
			recorder.Eventf(p, v1.EventTypeNormal, "DeletedByRescheduler",
				"Deleted by rescheduler in order to schedule critical pod %s.", podId(criticalPod))
			deleteOptions := metav1.DeleteOptions{}
			gracePeriodSeconds := int64(gracePeriod.Seconds())
			if gracePeriodSeconds >= 0 && (p.Spec.TerminationGracePeriodSeconds == nil || *p.Spec.TerminationGracePeriodSeconds > gracePeriodSeconds) {
				deleteOptions.GracePeriodSeconds = &gracePeriodSeconds
			}
			delErr := client.CoreV1().Pods(p.Namespace).Delete(p.Name, &deleteOptions)
			if delErr != nil {
				return fmt.Errorf("Failed to delete pod %s: %v", podId(p), delErr)
			}
			metrics.DeletedPodsCount.Inc()
		} else {
			newPods := append(nodeInfo.Pods(), p)
			nodeInfo = schedulercache.NewNodeInfo(newPods...)
			nodeInfo.SetNode(node)
		}
	}

	// TODO(piosz): how to reset scheduler backoff?
	return nil
}

func addTaint(client kube_client.Interface, node *v1.Node, value string) error {
	node.Spec.Taints = append(node.Spec.Taints, v1.Taint{
		Key:    criticalAddonsOnlyTaintKey,
		Value:  value,
		Effect: v1.TaintEffectNoSchedule,
	})

	if _, err := client.CoreV1().Nodes().Update(node); err != nil {
		return err
	}
	return nil
}

// Currently the logic choose a random node which satisfies requirements (a critical pod fits there).
// TODO(piosz): add a prioritization to this logic
func findNodeForPod(client kube_client.Interface, predicateChecker *ca_simulator.PredicateChecker, nodes []*v1.Node, pod *v1.Pod) *v1.Node {
	for _, node := range nodes {
		// ignore nodes with taints
		if err := checkTaints(node); err != nil {
			glog.Warningf("Skipping node %v due to %v", node.Name, err)
		}

		requiredPods, _, err := groupPods(client, node)
		if err != nil {
			glog.Warningf("Skipping node %v due to error: %v", node.Name, err)
			continue
		}

		nodeInfo := schedulercache.NewNodeInfo(requiredPods...)
		nodeInfo.SetNode(node)

		if err := predicateChecker.CheckPredicates(pod, nil, nodeInfo, true); err == nil {
			return node
		}
	}
	return nil
}

func checkTaints(node *v1.Node) error {
	for _, taint := range node.Spec.Taints {
		if taint.Key == criticalAddonsOnlyTaintKey {
			return fmt.Errorf("CriticalAddonsOnly taint with value: %v", taint.Value)
		}
	}
	return nil
}

// groupPods divides pods running on <node> into those which can't be deleted and the others
func groupPods(client kube_client.Interface, node *v1.Node) ([]*v1.Pod, []*v1.Pod, error) {
	podsOnNode, err := client.CoreV1().Pods(v1.NamespaceAll).List(
		metav1.ListOptions{FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": node.Name}).String()})
	if err != nil {
		return []*v1.Pod{}, []*v1.Pod{}, err
	}

	requiredPods := make([]*v1.Pod, 0)
	otherPods := make([]*v1.Pod, 0)
	for i := range podsOnNode.Items {
		pod := &podsOnNode.Items[i]

		//creatorRef, err := ca_drain.CreatorRefKind(pod)
		if err != nil {
			return []*v1.Pod{}, []*v1.Pod{}, err
		}

		if isMirrorPod(pod) || isDaemonsetPod(pod) || isCriticalPod(pod) {
			requiredPods = append(requiredPods, pod)
		} else {
			otherPods = append(otherPods, pod)
		}
	}

	return requiredPods, otherPods, nil
}

func filterCriticalDaemonSetPods(allPods []*v1.Pod, podsBeingProcessed *podSet) []*v1.Pod {
	criticalPods := []*v1.Pod{}
	for _, pod := range allPods {
		if isCriticalPod(pod) && isDaemonsetPod(pod) && !podsBeingProcessed.Has(pod) {
			criticalPods = append(criticalPods, pod)
		}
	}
	return criticalPods
}

func isCriticalPod(pod *v1.Pod) bool {
	return pod.Namespace == kubeapi.NamespaceSystem &&
		(isCritical(pod.Annotations) || (pod.Spec.Priority != nil && isCriticalPodBasedOnPriority(*pod.Spec.Priority)))
}

// isCritical returns true if parameters bear the critical pod annotation
func isCritical(annotations map[string]string) bool {
	val, ok := annotations[criticalPodAnnotation]
	if ok && val == "" {
		return true
	}
	return false
}

// isCriticalPodBasedOnPriority checks if the given pod is a critical pod based on priority resolved from pod Spec.
func isCriticalPodBasedOnPriority(priority int32) bool {
	if priority >= SystemCriticalPriority {
		return true
	}
	return false
}

// isMirrorPod checks whether the pod is a mirror pod.
func isMirrorPod(pod *v1.Pod) bool {
	_, found := pod.ObjectMeta.Annotations[types.ConfigMirrorAnnotationKey]
	return found
}

// isDaemonSetPod checks where the pod is a daemonset pod.
func isDaemonsetPod(pod *v1.Pod) bool {
	ownerRefList := pod.ObjectMeta.GetOwnerReferences()
	for _, ownerRef := range ownerRefList {
		if ownerRef.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}
