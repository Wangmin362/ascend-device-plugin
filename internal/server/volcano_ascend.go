package server

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/Project-HAMi/HAMi/pkg/util"
	"github.com/Project-HAMi/HAMi/pkg/util/client"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

const (
	PodAnnotationMaxLength = 1024 * 1024
)

func GetPendingPod(node string) (*v1.Pod, error) {
	podList, err := client.GetClient().CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	oldestPod := getOldestPod(podList.Items, node)
	if oldestPod == nil {
		return nil, fmt.Errorf("cannot get valid pod")
	}

	return oldestPod, nil
}

func getOldestPod(pods []v1.Pod, nodename string) *v1.Pod {
	if len(pods) == 0 {
		return nil
	}
	oldest := pods[0]
	for _, pod := range pods {
		if pod.Annotations[util.AssignedNodeAnnotations] == nodename {
			klog.V(4).Infof("pod %s, predicate time: %s", pod.Name, pod.Annotations[util.AssignedTimeAnnotations])
			if getPredicateTimeFromPodAnnotation(&oldest) > getPredicateTimeFromPodAnnotation(&pod) {
				oldest = pod
			}
		}
	}
	klog.V(4).Infof("oldest pod %#v, predicate time: %#v", oldest.Name,
		oldest.Annotations[util.AssignedTimeAnnotations])
	annotation := map[string]string{util.AssignedTimeAnnotations: strconv.FormatUint(math.MaxUint64, 10)}
	if err := util.PatchPodAnnotations(&oldest, annotation); err != nil {
		klog.Errorf("update pod %s failed, err: %v", oldest.Name, err)
		return nil
	}
	return &oldest
}

func getPredicateTimeFromPodAnnotation(pod *v1.Pod) uint64 {
	assumeTimeStr, ok := pod.Annotations[util.AssignedTimeAnnotations]
	if !ok {
		klog.Warningf("volcano not write timestamp, pod Name: %s", pod.Name)
		return math.MaxUint64
	}
	if len(assumeTimeStr) > PodAnnotationMaxLength {
		klog.Warningf("timestamp fmt invalid, pod Name: %s", pod.Name)
		return math.MaxUint64
	}
	predicateTime, err := strconv.ParseUint(assumeTimeStr, 10, 64)
	if err != nil {
		klog.Errorf("parse timestamp failed, %v", err)
		return math.MaxUint64
	}
	return predicateTime
}
