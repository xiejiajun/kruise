package scale

import (
	"sort"

	appspub "github.com/openkruise/kruise/apis/apps/pub"
	appsv1alpha1 "github.com/openkruise/kruise/apis/apps/v1alpha1"
	"github.com/openkruise/kruise/pkg/util/lifecycle"
	"github.com/openkruise/kruise/pkg/util/specifieddelete"
	v1 "k8s.io/api/core/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"
	kubecontroller "k8s.io/kubernetes/pkg/controller"
	"k8s.io/utils/integer"
)

func isPodSpecifiedDelete(cs *appsv1alpha1.CloneSet, pod *v1.Pod) bool {
	if specifieddelete.IsSpecifiedDelete(pod) {
		// TODO 用户设置了apps.kruise.io/specified-delete=true标签的Pod直接可以删除
		return true
	}
	for _, name := range cs.Spec.ScaleStrategy.PodsToDelete {
		// TODO CloneSet扩容策略的podsToDelete设置的Pod要删除
		if name == pod.Name {
			return true
		}
	}
	return false
}

func getPlannedDeletedPods(cs *appsv1alpha1.CloneSet, pods []*v1.Pod) ([]*v1.Pod, []*v1.Pod) {
	var podsSpecifiedToDelete []*v1.Pod
	var podsInPreDelete []*v1.Pod
	for _, pod := range pods {
		if isPodSpecifiedDelete(cs, pod) {
			podsSpecifiedToDelete = append(podsSpecifiedToDelete, pod)
		}
		if lifecycle.GetPodLifecycleState(pod) == appspub.LifecycleStatePreparingDelete {
			// TODO 状态为预删除状态的需要删除
			podsInPreDelete = append(podsInPreDelete, pod)
		}
	}
	return podsSpecifiedToDelete, podsInPreDelete
}

// Get available IDs, if the a PVC exists but the corresponding pod does not exist, then reusing the ID, i.e., reuse the pvc.
// If there is not enough existing available IDs, then generate ID using rand utility.
// More details: if template changes more than container image, controller will delete pod during update, and
// it will keep the pvc to reuse.
func getOrGenAvailableIDs(num int, pods []*v1.Pod, pvcs []*v1.PersistentVolumeClaim) sets.String {
	existingIDs := sets.NewString()
	availableIDs := sets.NewString()
	for _, pvc := range pvcs {
		if id := pvc.Labels[appsv1alpha1.CloneSetInstanceID]; len(id) > 0 {
			existingIDs.Insert(id)
			availableIDs.Insert(id)
		}
	}

	for _, pod := range pods {
		if id := pod.Labels[appsv1alpha1.CloneSetInstanceID]; len(id) > 0 {
			existingIDs.Insert(id)
			availableIDs.Delete(id)
		}
	}

	retIDs := sets.NewString()
	for i := 0; i < num; i++ {
		id := getOrGenInstanceID(existingIDs, availableIDs)
		retIDs.Insert(id)
	}

	return retIDs
}

func getOrGenInstanceID(existingIDs, availableIDs sets.String) string {
	id, _ := availableIDs.PopAny()
	if len(id) == 0 {
		for {
			id = rand.String(LengthOfInstanceID)
			if !existingIDs.Has(id) {
				break
			}
		}
	}
	return id
}

// totalPods: 活跃的Pod数， notUpdatedPods: 等待更新的Pod总数
func calculateDiffs(cs *appsv1alpha1.CloneSet, totalPods int, notUpdatedPods int) (totalDiff int, currentRevDiff int) {
	var partition int
	if cs.Spec.UpdateStrategy.Partition != nil {
		// 灰度分批发布需要保留的老版本数
		partition, _ = intstrutil.GetValueFromIntOrPercent(cs.Spec.UpdateStrategy.Partition, int(*cs.Spec.Replicas), true)
	}

	if partition >= int(*cs.Spec.Replicas) {
		// TODO 当前活跃的Pod数 - CloneSet期望副本数得到需要更新的Pod数：totalPods只会小于等于期望副本数
		totalDiff = totalPods - int(*cs.Spec.Replicas)
		// TODO 还没更新的Pod数 - 期望副本数: notUpdatedPods也只会小于等于期望副本数
		//  也就是partition数大于期望副本数的适合，如果已经更新过一部分了，notUpdatedPods会小于期望副本数，这时候currentRevDiff会是负值
		//  但是如果活跃的副本数已经达到期望副本数的话，从realControl.Manage方法的逻辑可以知道是不会触发任何更新的
		currentRevDiff = notUpdatedPods - int(*cs.Spec.Replicas)
		return
	}

	var maxSurge int
	// TODO 本次要更新的 = 还未更新的 - partition
	currentRevDiff = notUpdatedPods - partition
	// Use maxSurge only if partition has not satisfied
	if currentRevDiff > 0 {
		if cs.Spec.UpdateStrategy.MaxSurge != nil {
			maxSurge, _ = intstrutil.GetValueFromIntOrPercent(cs.Spec.UpdateStrategy.MaxSurge, int(*cs.Spec.Replicas), true)
			maxSurge = integer.IntMin(maxSurge, currentRevDiff)
		}
	}

	totalDiff = totalPods - int(*cs.Spec.Replicas) - maxSurge
	if totalDiff != 0 && maxSurge > 0 {
		klog.V(3).Infof("CloneSet scale diff(%d),currentRevDiff(%d) with maxSurge %d", totalDiff, currentRevDiff, maxSurge)
	}
	return
}

func choosePodsToDelete(totalDiff int, currentRevDiff int, notUpdatedPods, updatedPods []*v1.Pod) []*v1.Pod {
	choose := func(pods []*v1.Pod, diff int) []*v1.Pod {
		// No need to sort pods if we are about to delete all of them.
		if diff < len(pods) {
			// Sort the pods in the order such that not-ready < ready, unscheduled
			// < scheduled, and pending < running. This ensures that we delete pods
			// in the earlier stages whenever possible.
			sort.Sort(kubecontroller.ActivePods(pods))
		} else if diff > len(pods) {
			klog.Warningf("Diff > len(pods) in choosePodsToDelete func which is not expected.")
			return pods
		}
		return pods[:diff]
	}

	var podsToDelete []*v1.Pod
	if currentRevDiff >= totalDiff {
		podsToDelete = choose(notUpdatedPods, totalDiff)
	} else if currentRevDiff > 0 {
		podsToDelete = choose(notUpdatedPods, currentRevDiff)
		podsToDelete = append(podsToDelete, choose(updatedPods, totalDiff-currentRevDiff)...)
	} else {
		podsToDelete = choose(updatedPods, totalDiff)
	}

	return podsToDelete
}
