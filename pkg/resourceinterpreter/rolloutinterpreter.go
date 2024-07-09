package resourceinterpreter

import (
	"encoding/json"
	"fmt"
	"reflect"

	configv1alpha1 "github.com/karmada-io/karmada/pkg/apis/config/v1alpha1"
	workv1alpha1 "github.com/karmada-io/karmada/pkg/apis/work/v1alpha1"
	workv1alpha2 "github.com/karmada-io/karmada/pkg/apis/work/v1alpha2"
	"github.com/karmada-io/karmada/pkg/util"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/integer"
	"k8s.io/utils/pointer"
)

const AnnotationKeyPodTemplateHash = "rsi.xhs.com/pod-template-hash"

type UnifiedWorkload struct {
	DesiredReplicas int32
	CurrentReplicas int32
	RollingReplicas int32
	SpecRevision    string
	ClusterName     string
	RollingStrategy *configv1alpha1.RollingStrategy
	Status          *configv1alpha1.UnifiedRollingStatus
}

type RolloutInterpreter interface {
	AlignRollingStrategy(resourceTemplate *unstructured.Unstructured, workloads map[string]*unstructured.Unstructured, works map[string]*workv1alpha1.Work) (map[string]*unstructured.Unstructured, error)
}

type statelessRolloutInterpreterImpl struct {
	ResourceInterpreter
}

// func (i *statelessRolloutInterpreterImpl) AlignRollingStrategy(resourceTemplate *unstructured.Unstructured, workloads map[string]*unstructured.Unstructured, clusters []workv1alpha2.TargetCluster, works map[string]*workv1alpha1.Work) (map[string]*unstructured.Unstructured, error) {
func (i *statelessRolloutInterpreterImpl) AlignRollingStrategy(resourceTemplate *unstructured.Unstructured, clusters []workv1alpha2.TargetCluster, works map[string]*workv1alpha1.Work) (map[string]*configv1alpha1.RollingStrategy, error) {
	target, source, err := func() (*UnifiedWorkload, map[string]*UnifiedWorkload, error) {
		replicas, _, err := i.GetReplicas(resourceTemplate)
		if err != nil {
			return nil, nil, err
		}

		strategy, err := i.GetRollingStrategy(resourceTemplate)
		if err != nil {
			return nil, nil, err
		}

		specRevision := util.GetAnnotationValue(resourceTemplate.GetAnnotations(), AnnotationKeyPodTemplateHash)
		target := &UnifiedWorkload{
			DesiredReplicas: replicas,
			RollingStrategy: strategy,
			SpecRevision:    specRevision,
		}

		target.RollingStrategy.Partition, err = transIntValueIfNeeds(target.DesiredReplicas, target.RollingStrategy.Partition)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to translate partition value: %v", err)
		}

		target.RollingStrategy.MaxUnavailable, err = transIntValueIfNeeds(target.DesiredReplicas, target.RollingStrategy.MaxUnavailable)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to translate max-unavailable value: %v", err)
		}

		target.RollingStrategy.MaxSurge, err = transIntValueIfNeeds(target.DesiredReplicas, target.RollingStrategy.MaxSurge)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to translate max-surge value: %v", err)
		}

		if target.RollingStrategy.MaxUnavailable.IntVal == 0 && target.RollingStrategy.MaxSurge.IntVal == 0 {
			target.RollingStrategy.MaxUnavailable.IntVal = 1
		}

		targetCluster := make(map[string]int32, len(clusters))
		for _, cluster := range clusters {
			targetCluster[cluster.Name] = cluster.Replicas
		}

		source := make(map[string]*UnifiedWorkload, len(clusters))
		for cluster, work := range works {
			if _, ok := targetCluster[cluster]; !ok {
				continue
			}

			source[cluster] = &UnifiedWorkload{
				ClusterName: cluster,
			}

			workload := unstructured.Unstructured{}
			if err = json.Unmarshal(work.Spec.Workload.Manifests[0].Raw, &workload); err != nil {
				return nil, nil, err
			}

			status, err := i.InterpretRollingStatus(&workload, work.Status.ManifestStatuses[0].Status)
			if err != nil {
				return nil, nil, err
			}
			source[cluster].Status = status

			if status.Generation != status.ObservedGeneration {
				return nil, nil, fmt.Errorf("workload %s/%s has not been reconciled", workload.GetNamespace(), workload.GetName())
			}

			current, _, err := i.GetReplicas(&workload)
			if err != nil {
				return nil, nil, err
			}
			source[cluster].CurrentReplicas = current

			strategy, err = i.GetRollingStrategy(&workload)
			if err != nil {
				return nil, nil, err
			}
			source[cluster].RollingStrategy = strategy
			source[cluster].SpecRevision = util.GetAnnotationValue(workload.GetAnnotations(), AnnotationKeyPodTemplateHash)
		}

		for cluster, desired := range targetCluster {
			workload, ok := source[cluster]
			if !ok {
				workload = &UnifiedWorkload{
					ClusterName:     cluster,
					CurrentReplicas: 0,
					DesiredReplicas: desired,
					SpecRevision:    specRevision,
				}
			}
			SetDefaultUnifiedWorkload(workload, target)
			source[cluster] = workload
		}
		return target, source, nil
	}()
	if err != nil {
		return nil, err
	}

	wlList := make([]*UnifiedWorkload, 0, len(source))
	updateTargetDelta := target.DesiredReplicas - target.RollingStrategy.Partition.IntVal
	for cluster, wl := range source {
		if _, ok := works[cluster]; !ok {
			updateTargetDelta -= wl.DesiredReplicas
			continue
		}

		if wl.RollingStrategy.Partition != nil && wl.RollingStrategy.Partition.IntVal > 0 {
			// maxUpdatedReplicasPossible is the maximum number of replicas that this workload may update.
			// currentPartition should equals to `target.Replicas - maxUpdatedReplicasPossible`, which means
			// we expected the newly-scaled-up replicas is the old revision as much as possible if
			// `target.Replicas > currentReplicas`.
			maxUpdatedReplicasPossible := integer.Int32Max(wl.CurrentReplicas-wl.RollingStrategy.Partition.IntVal, 0)
			maxUpdatedReplicasPossible = integer.Int32Min(maxUpdatedReplicasPossible, wl.CurrentReplicas)
			maxUpdatedReplicasPossible = integer.Int32Min(maxUpdatedReplicasPossible, wl.DesiredReplicas)
			wl.RollingStrategy.Partition = &intstr.IntOrString{Type: intstr.Int, IntVal: wl.DesiredReplicas - maxUpdatedReplicasPossible}
		} else {
			wl.RollingStrategy.Partition = &intstr.IntOrString{Type: intstr.Int, IntVal: 0}
		}

		// for new-revision workload, we should consider it have been updated partially.
		// for old-revision workload, we should assume it is fully partitioned.
		if wl.SpecRevision == target.SpecRevision {
			updateTargetDelta -= wl.DesiredReplicas - int32(wl.RollingStrategy.Partition.IntValue())
		} else {
			wl.RollingStrategy.Partition.IntVal = wl.DesiredReplicas
		}
		wlList = append(wlList, wl)
	}

	// ensure update unavailable replicas first
	for _, wl := range wlList {
		newPartition := &intstr.IntOrString{Type: intstr.Int, IntVal: 0}
		if wl.RollingStrategy.Partition != nil {
			newPartition.IntVal = wl.RollingStrategy.Partition.IntVal
		}
		updatedUnavailable := integer.Int32Min(*wl.Status.UpdatedReplicas-*wl.Status.UpdatedReadyReplicas, 0)
		currentUnavailable := integer.Int32Max(wl.DesiredReplicas-*wl.Status.AvailableReplicas-updatedUnavailable, 0)
		// the number of next updated replicas in this member cluster should
		// not exceed the target updated replicas at federated level.
		nextUpdate := integer.Int32Min(newPartition.IntVal, updateTargetDelta)
		nextUpdate = integer.Int32Min(nextUpdate, currentUnavailable)
		newPartition.IntVal -= nextUpdate
		updateTargetDelta -= nextUpdate
		wl.RollingStrategy.Partition = newPartition
	}

	// ensure partition is correctly set
	for _, wl := range wlList {
		newPartition := wl.RollingStrategy.Partition
		// the number of next updated replicas in this member cluster should
		// not exceed the target updated replicas at federated level.
		nextUpdate := integer.Int32Min(newPartition.IntVal, updateTargetDelta)
		newPartition.IntVal -= nextUpdate
		updateTargetDelta -= nextUpdate
		wl.RollingStrategy.Partition = newPartition
	}

	unavailableQuota := target.RollingStrategy.MaxUnavailable.IntVal
	for _, wl := range wlList {
		unavailableReplicas := integer.Int32Max(wl.DesiredReplicas-*wl.Status.AvailableReplicas, 0)
		initializedMaxUnavailable := integer.Int32Min(unavailableReplicas, unavailableQuota)
		wl.RollingStrategy.MaxUnavailable.IntVal = initializedMaxUnavailable
		unavailableQuota -= initializedMaxUnavailable
	}

	surgeQuota := target.RollingStrategy.MaxSurge.IntVal
	for _, wl := range wlList {
		unavailableReplicas := integer.Int32Max(wl.DesiredReplicas-*wl.Status.AvailableReplicas, 0)
		rollingReplicas := integer.Int32Max(wl.DesiredReplicas-wl.RollingStrategy.Partition.IntVal, 0)
		currentAvailableToUpdate := integer.Int32Max(rollingReplicas-*wl.Status.UpdatedReadyReplicas-unavailableReplicas, 0)
		unavailableQuotaToAdd := integer.Int32Min(currentAvailableToUpdate, unavailableQuota)
		wl.RollingStrategy.MaxUnavailable.IntVal += unavailableQuotaToAdd
		unavailableQuota -= unavailableQuotaToAdd

		surgeQuotaToAdd := integer.Int32Min(wl.RollingReplicas, surgeQuota)
		wl.RollingStrategy.MaxSurge.IntVal = surgeQuotaToAdd
		surgeQuota -= surgeQuotaToAdd
	}

	for _, wl := range wlList {
		if wl.RollingStrategy.MaxSurge.IntVal == 0 && wl.RollingStrategy.MaxUnavailable.IntVal == 0 {
			wl.RollingStrategy.MaxUnavailable.IntVal = 1
			wl.RollingStrategy.Partition.IntVal = wl.DesiredReplicas
		}
	}

	revised := make(map[string]*configv1alpha1.RollingStrategy, len(wlList))
	for _, wl := range wlList {
		revised[wl.ClusterName] = wl.RollingStrategy
	}
	return revised, nil

	//revised := make(map[string]*unstructured.Unstructured, len(wlList))
	//for _, wl := range wlList {
	//	revised[wl.ClusterName], err = i.ReviseRollingStrategy(workloads[wl.ClusterName], wl.RollingStrategy)
	//	if err != nil {
	//		return nil, err
	//	}
	//}
	//return revised, nil
}

func SetDefaultUnifiedWorkload(wl, target *UnifiedWorkload) {
	if wl.RollingStrategy == nil {
		wl.RollingStrategy = &configv1alpha1.RollingStrategy{
			Partition:      &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
			MaxSurge:       &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
			MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
		}
	}
	if wl.RollingStrategy.Partition == nil {
		wl.RollingStrategy.Partition = &intstr.IntOrString{Type: intstr.Int, IntVal: 0}
	}
	if wl.RollingStrategy.MaxUnavailable == nil {
		wl.RollingStrategy.MaxUnavailable = &intstr.IntOrString{Type: intstr.Int, IntVal: 0}
	}
	if wl.RollingStrategy.MaxSurge == nil {
		wl.RollingStrategy.MaxSurge = &intstr.IntOrString{Type: intstr.Int, IntVal: 0}
	}

	if wl.Status == nil {
		wl.Status = &configv1alpha1.UnifiedRollingStatus{}
	}
	if wl.Status.Replicas == nil {
		wl.Status.Replicas = pointer.Int32(0)
	}
	if wl.Status.ReadyReplicas == nil {
		wl.Status.ReadyReplicas = pointer.Int32(0)
	}
	if wl.Status.AvailableReplicas == nil {
		wl.Status.AvailableReplicas = pointer.Int32(0)
	}
	if wl.Status.UpdatedReplicas == nil {
		wl.Status.UpdatedReplicas = pointer.Int32(0)
	}
	if wl.SpecRevision != target.SpecRevision {
		wl.Status.UpdatedReplicas = pointer.Int32(0)
		wl.Status.UpdatedReadyReplicas = pointer.Int32(0)
	}
}

func transIntValueIfNeeds(replicas int32, partition *intstr.IntOrString) (*intstr.IntOrString, error) {
	if partition.Type == intstr.Int {
		return partition, nil
	}
	intValue, err := intstr.GetScaledValueFromIntOrPercent(partition, int(replicas), true)
	if err != nil {
		return nil, err
	}
	return &intstr.IntOrString{Type: intstr.Int, IntVal: int32(intValue)}, nil
}

func IsConsistentRevision(workloadI, workloadJ *unstructured.Unstructured) bool {
	t1, _, _ := unstructured.NestedFieldNoCopy(workloadI.Object, "spec", "template")
	t2, _, _ := unstructured.NestedFieldNoCopy(workloadJ.Object, "spec", "template")
	return reflect.DeepEqual(t1, t2)
}
