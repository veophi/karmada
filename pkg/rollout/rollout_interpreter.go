package rollout

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	configv1alpha1 "github.com/karmada-io/karmada/pkg/apis/config/v1alpha1"
	workv1alpha1 "github.com/karmada-io/karmada/pkg/apis/work/v1alpha1"
	workv1alpha2 "github.com/karmada-io/karmada/pkg/apis/work/v1alpha2"
	"github.com/karmada-io/karmada/pkg/resourceinterpreter"
	"github.com/karmada-io/karmada/pkg/util"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"k8s.io/utils/integer"
)

// Interpreter is the interface for aligning rolling strateg.
type Interpreter interface {
	AlignRollingStrategy(resourceTemplate *unstructured.Unstructured, workloads map[string]*unstructured.Unstructured, clusters []workv1alpha2.TargetCluster, works map[string]*workv1alpha1.Work) (map[string]*unstructured.Unstructured, bool, error)
}

// statelessRolloutInterpreterImpl is the implementation of Rollout Interpreter for stateless workloads.
type statelessRolloutInterpreterImpl struct { // 这个实现是通用的还是转为stateless设计的呢？
	resourceInterpreter resourceinterpreter.ResourceInterpreter
}

// NewAdapterRolloutInterpreter creates a new Rollout Interpreter for given workload type.
func NewAdapterRolloutInterpreter(interpreter resourceinterpreter.ResourceInterpreter) Interpreter {
	// TODO: support stateful rolling interpreter type
	return &statelessRolloutInterpreterImpl{resourceInterpreter: interpreter}
}

// AlignRollingStrategy aligns the rolling strategy for each member based on the given resource template and workloads status.
func (i *statelessRolloutInterpreterImpl) AlignRollingStrategy(resourceTemplate *unstructured.Unstructured, workloads map[string]*unstructured.Unstructured, clusters []workv1alpha2.TargetCluster, works map[string]*workv1alpha1.Work) (map[string]*unstructured.Unstructured, bool, error) {
	clusterRolloutStrategies, stopReconcile, err := i.calculateRollingStrategyForEachCluster(resourceTemplate, clusters, works)
	if err != nil || stopReconcile {
		return nil, stopReconcile, err
	}

	revisedWorkloads := make(map[string]*unstructured.Unstructured, len(workloads))
	for clusterName, workload := range workloads {
		revisedWorkloads[clusterName], err = i.resourceInterpreter.ReviseRollingStrategy(workload, clusterRolloutStrategies[clusterName])
		if err != nil {
			klog.Errorf("Failed to revise workload(%s/%s /%s/%s) under cluster(%s), error: %v",
				workload.GetAPIVersion(), workload.GetKind(), workload.GetNamespace(), workload.GetName(), clusterName, err)
			return nil, true, err
		}
	}
	return revisedWorkloads, false, nil
}

// calculateRollingStrategyForEachCluster calculates the rolling strategy for each member based on the given resource template and workloads status.
func (i *statelessRolloutInterpreterImpl) calculateRollingStrategyForEachCluster(resourceTemplate *unstructured.Unstructured, clusters []workv1alpha2.TargetCluster, works map[string]*workv1alpha1.Work) (map[string]*configv1alpha1.RollingStrategy, bool, error) {
	klog.V(4).Infof("Begin to calculate rollingStrategy for resource(%s/%s %s/%s).",
		resourceTemplate.GetAPIVersion(), resourceTemplate.GetKind(), resourceTemplate.GetNamespace(), resourceTemplate.GetName())

	federationContext, memberContexts, stopReconcile, err := i.calculateRollingContext(resourceTemplate, clusters, works)
	if err != nil {
		klog.Errorf("Failed to calculate rollingStrategy contexts for resource(%s/%s): %v",
			resourceTemplate.GetNamespace(), resourceTemplate.GetName(), err)
		return nil, stopReconcile, err
	}
	if stopReconcile {
		return nil, stopReconcile, nil
	}

	memberContexts = i.calculatePartitionsForEachCluster(federationContext, memberContexts, works)
	memberContexts = i.calculateMaxUnavailableForEachCluster(federationContext, memberContexts)
	memberContexts = i.calculateMaxSurgeForEachCluster(federationContext, memberContexts)

	if err = checkRollingStrategyAligned(federationContext, memberContexts); err != nil {
		// Just warning it, no need to return this error.
		klog.Warningf("Rolling strategy is not aligned for resource(%s/%s): %v",
			resourceTemplate.GetNamespace(), resourceTemplate.GetName(), err)
	}

	memberStrategies := i.correctAndGenerateValidRollingStrategy(memberContexts)
	klog.V(4).Infof("Calculated rolling strategies for resource(%s/%s) in memberContexts: %#v",
		resourceTemplate.GetNamespace(), resourceTemplate.GetName(), memberStrategies)
	return memberStrategies, false, nil

}

// calculateRollingContext calculates the rolling context for federation and members.
func (i *statelessRolloutInterpreterImpl) calculateRollingContext(resourceTemplate *unstructured.Unstructured, clusters []workv1alpha2.TargetCluster, works map[string]*workv1alpha1.Work) (*rollingContext, []*rollingContext, bool, error) {
	fedRollingContext, err := i.calculateFederationRollingContext(resourceTemplate)
	if err != nil {
		klog.Errorf("Failed to calculate federation rollingStrategy contexts for resource(%s/%s): %v",
			resourceTemplate.GetNamespace(), resourceTemplate.GetName(), err)
		return nil, nil, false, err
	}

	memberRollingContexts, stopReconcile, err := i.calculateMemberRollingContexts(resourceTemplate, fedRollingContext, works, clusters)
	if err != nil {
		klog.Errorf("Failed to calculate member rollingStrategy contexts for resource(%s/%s): %v",
			resourceTemplate.GetNamespace(), resourceTemplate.GetName(), err)
		return nil, nil, false, err
	}

	if stopReconcile {
		// Just stop this reconcile round, wait the condition met and trigger the next reconcile.
		return nil, nil, stopReconcile, nil
	}
	return fedRollingContext, memberRollingContexts, false, nil
}

func (i *statelessRolloutInterpreterImpl) calculateFederationRollingContext(resourceTemplate *unstructured.Unstructured) (*rollingContext, error) {
	replicas, _, err := i.resourceInterpreter.GetReplicas(resourceTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to get replicas: %v", err)
	}

	rollingStrategy, err := i.resourceInterpreter.GetRollingStrategy(resourceTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to get rolling rollingStrategy: %v", err)
	}

	rollingStrategy, err = normalizeRollingStrategy(rollingStrategy, replicas)
	if err != nil {
		return nil, fmt.Errorf("failed to normalize rollingStrategy: %v", err)
	}

	// MaxUnavailable and MaxSurge should not be both 0, we should ensure at least one of replicas can be updated.
	// This constraint will be validated by APIServer or workload operators, if we do not correct it, it will be
	// an undefined behavior.
	if replicas > 0 && rollingStrategy.MaxUnavailable.IntVal == 0 && rollingStrategy.MaxSurge.IntVal == 0 {
		rollingStrategy.MaxUnavailable.IntVal = 1
	}

	fedRollingContext := &rollingContext{
		DesiredReplicas: replicas,
		RollingStrategy: rollingStrategy,
		SpecRevision:    util.GetAnnotationValue(resourceTemplate.GetAnnotations(), workv1alpha2.ResourceTemplateTemplateHashAnnotationKey),
	}

	klog.V(4).Infof("Calculated federation rollingStrategy context for resource(%s/%s): %+v",
		resourceTemplate.GetNamespace(), resourceTemplate.GetName(), fedRollingContext)
	return fedRollingContext, nil
}

func (i *statelessRolloutInterpreterImpl) calculateMemberRollingContexts(resourceTemplate *unstructured.Unstructured, fedRollingContext *rollingContext, works map[string]*workv1alpha1.Work, clusters []workv1alpha2.TargetCluster) ([]*rollingContext, bool, error) {
	clusterReplicas := make(map[string]int32, len(clusters))
	for _, cluster := range clusters {
		clusterReplicas[cluster.Name] = cluster.Replicas
	}

	rollingContextMap := make(map[string]*rollingContext, len(clusters))
	for clusterName, work := range works {
		if _, ok := clusterReplicas[clusterName]; !ok {
			continue
		}

		manifest, status := unstructured.Unstructured{}, &configv1alpha1.UnifiedRollingStatus{}
		if err := i.decodeWorkTo(work, &manifest, status); err != nil {
			return nil, false, fmt.Errorf("failed to decode workork  %s/%s in cluster(%s): %v", resourceTemplate.GetNamespace(), resourceTemplate.GetName(), clusterName, err)
		}

		// If workload in this member cluster is inconsistent with its manifests,
		// we should stop reconcile and wait it to be reconciled by its execution
		// controller and its native controller in single-cluster.
		if !isWorkloadGenerationConsistent(&manifest, status) {
			// No need to return error, wait its status changes to trigger the next reconcile.
			klog.Infof("Current work mainifest %s/%s has not been reconciled in cluster(%s), stop reconcile.", resourceTemplate.GetNamespace(), resourceTemplate.GetName(), clusterName)
			return nil, true, nil
		}

		currentReplicas, _, err := i.resourceInterpreter.GetReplicas(&manifest)
		if err != nil {
			return nil, false, fmt.Errorf("failed to get replicas for workload: %v", err)
		}

		rollingStrategy, err := i.resourceInterpreter.GetRollingStrategy(&manifest)
		if err != nil {
			return nil, false, fmt.Errorf("failed to get rolling rollingStrategy for workload: %v", err)
		}

		rollingContextMap[clusterName] = &rollingContext{
			ClusterName:     clusterName,
			Status:          status,
			CurrentReplicas: currentReplicas,
			RollingStrategy: rollingStrategy,
			SpecRevision:    util.GetAnnotationValue(manifest.GetAnnotations(), workv1alpha2.ResourceTemplateTemplateHashAnnotationKey),
		}
	}

	memberRollingContexts := make([]*rollingContext, 0, len(clusterReplicas))
	for cluster, desiredReplicas := range clusterReplicas {
		memberContext, ok := rollingContextMap[cluster]
		if !ok || memberContext == nil {
			memberContext = &rollingContext{
				CurrentReplicas: 0,
				ClusterName:     cluster,
				SpecRevision:    fedRollingContext.SpecRevision,
			}
		}
		memberContext, err := normalizeRollingContext(memberContext, fedRollingContext, desiredReplicas)
		if err != nil {
			return nil, false, fmt.Errorf("failed to set default unified workload: %v", err)
		}
		memberRollingContexts = append(memberRollingContexts, memberContext)
	}
	sort.Sort(desiredReplicasLess(memberRollingContexts))

	klog.V(4).Infof("Calculated rolling context for %s/%s in members: %+v",
		resourceTemplate.GetNamespace(), resourceTemplate.GetName(), memberRollingContexts)
	return memberRollingContexts, false, nil
}

func (i *statelessRolloutInterpreterImpl) decodeWorkTo(work *workv1alpha1.Work, manifest *unstructured.Unstructured, status *configv1alpha1.UnifiedRollingStatus) error {
	err := json.Unmarshal(work.Spec.Workload.Manifests[0].Raw, manifest)
	if err != nil {
		return fmt.Errorf("failed to unmarshal workload from work: %v", err)
	}

	if len(work.Status.ManifestStatuses) > 0 {
		status, err = i.resourceInterpreter.InterpretRollingStatus(manifest, work.Status.ManifestStatuses[0].Status)
		if err != nil {
			return fmt.Errorf("failed to interpret rolling status: %v", err)
		}
	}
	return nil
}

func (i *statelessRolloutInterpreterImpl) calculatePartitionsForEachCluster(federationContext *rollingContext, memberContexts []*rollingContext, works map[string]*workv1alpha1.Work) []*rollingContext {
	// updatedReplicasQuota indicate the number of **oldRevision** replicas
	// that can be updated in federated level according to `partition`.
	//
	// initialized by  `SUM(memberContext.replicasMember.partition) - federationContext.partition`
	var updatedReplicasQuota = federationContext.DesiredReplicas - federationContext.RollingStrategy.Partition.IntVal
	for _, memberContext := range memberContexts {
		// The corresponding work is not applied means all of it revision must be update revision.
		// All replicas of this memberContext will be updated.
		if _, ok := works[memberContext.ClusterName]; !ok {
			updatedReplicasQuota -= memberContext.DesiredReplicas
			continue
		}

		// correct the partition for this memberContext.
		memberContext.RollingStrategy.Partition.IntVal = integer.Int32Min(memberContext.RollingStrategy.Partition.IntVal, memberContext.OldRevisionReplicas())

		// We correct the current partition for this memberContext.
		if memberContext.RollingStrategy.Partition.IntVal > 0 {
			// TODO: decide to scale new/old revision pod by partition.
			// maxUpdatedReplicasPossible is the maximum number of replicas that this workload may update.
			// current Partition should equal to `target.Replicas - maxUpdatedReplicasPossible`, which means
			// we expected the newly-scaled-up replicas is the old revision as much as possible if
			// `target.Replicas > currentReplicas`.
			maxUpdatedReplicasPossible := integer.Int32Max(memberContext.CurrentReplicas-memberContext.RollingStrategy.Partition.IntVal, 0)
			maxUpdatedReplicasPossible = integer.Int32Min(maxUpdatedReplicasPossible, memberContext.DesiredReplicas)
			partitionedReplicasPossible := integer.Int32Max(memberContext.DesiredReplicas, memberContext.CurrentReplicas) - maxUpdatedReplicasPossible
			memberContext.RollingStrategy.Partition = &intstr.IntOrString{Type: intstr.Int, IntVal: partitionedReplicasPossible}
		}

		if memberContext.SpecRevision == federationContext.SpecRevision {
			// for new-revision workload, we should consider it have been updated partially.
			updatedReplicasQuota -= memberContext.DesiredReplicas - int32(memberContext.RollingStrategy.Partition.IntValue())
		} else {
			// for old-revision workload, we should assume it is fully partitioned.
			memberContext.RollingStrategy.Partition.IntVal = memberContext.DesiredReplicas
		}
	}

	if updatedReplicasQuota <= 0 {
		return memberContexts
	}

	// memberContext.partition += MIN(updatedReplicasQuota, memberContext.currentUnavailable)
	//
	// This loop ensure update the old unavailable replicas firstly.
	for _, member := range memberContexts {
		if updatedReplicasQuota == 0 {
			break
		}

		updatedUnavailable := member.UpdatedUnavailableReplicas()
		currentUnavailable := integer.Int32Max(*member.Status.Replicas-*member.Status.AvailableReplicas-updatedUnavailable, 0)
		updatedReplicasQuotaToAdd := integer.Int32Min(member.RollingStrategy.Partition.IntVal, updatedReplicasQuota)
		updatedReplicasQuotaToAdd = integer.Int32Min(updatedReplicasQuotaToAdd, currentUnavailable)
		member.RollingStrategy.Partition.IntVal -= updatedReplicasQuotaToAdd
		updatedReplicasQuota -= updatedReplicasQuotaToAdd
	}

	if updatedReplicasQuota <= 0 {
		return memberContexts
	}

	// memberContext.partition += MIN(updateQuota, memberContext.current)
	//
	// This loop ensure sum of memberContext target update replicas equals to federationContext's.
	for _, member := range memberContexts {
		if updatedReplicasQuota == 0 {
			break
		}
		// the number of next updated replicas in this memberContext cluster should
		// not exceed the target updated replicas at federated level.
		updatedReplicasQuotaToAdd := integer.Int32Min(member.RollingStrategy.Partition.IntVal, updatedReplicasQuota)
		member.RollingStrategy.Partition.IntVal -= updatedReplicasQuotaToAdd
		updatedReplicasQuota -= updatedReplicasQuotaToAdd
	}

	return memberContexts
}

func (i *statelessRolloutInterpreterImpl) calculateMaxUnavailableForEachCluster(federationContext *rollingContext, memberContexts []*rollingContext) []*rollingContext {
	// updateTargetQuota indicate the number of **old-revision && ready** replicas
	// that can be updated in federated level according to `maxUnavailable`.
	//
	// initialized by federationContext.maxUnavailable - SUM(member.unavailable)
	var unavailableQuota = federationContext.RollingStrategy.MaxUnavailable.IntVal
	for _, member := range memberContexts {
		unavailableReplicas := integer.Int32Max(member.DesiredReplicas-*member.Status.AvailableReplicas, 0)
		unavailableQuotaToAdd := integer.Int32Min(unavailableReplicas, unavailableQuota)
		member.RollingStrategy.MaxUnavailable.IntVal = unavailableQuotaToAdd
		unavailableQuota -= unavailableQuotaToAdd
	}

	if unavailableQuota <= 0 {
		return memberContexts
	}

	// member.maxUnavailable += MIN(unavailableQuota, member.current_available_to_update)
	//
	// This loop ensure not exceed federationContext.maxUnavailable by controlling the number of available replicas to update.
	for _, member := range memberContexts {
		if unavailableQuota == 0 {
			break
		}
		currentRevisionReplicas := integer.Int32Max(*member.Status.Replicas-*member.Status.UpdatedReplicas, 0)
		currentToUpdateReplicas := integer.Int32Max(currentRevisionReplicas-member.RollingStrategy.Partition.IntVal, 0)

		var unavailableQuotaToAdd int32
		if member.Status.UpdatedReadyReplicas != nil {
			// more accurate calculation for such workload reporting its updated ready replicas.
			unavailableReplicas := integer.Int32Max(*member.Status.Replicas-*member.Status.AvailableReplicas, 0)
			currentUnavailableReplicas := integer.Int32Max(unavailableReplicas-member.UpdatedUnavailableReplicas(), 0)
			currentAvailableReplicas := integer.Int32Max(currentToUpdateReplicas-currentUnavailableReplicas, 0)
			unavailableQuotaToAdd = integer.Int32Min(currentAvailableReplicas, unavailableQuota)
		} else {
			unavailableQuotaToAdd = integer.Int32Min(currentToUpdateReplicas, unavailableQuota)
			unavailableQuotaToAdd = integer.Int32Min(unavailableQuotaToAdd, *member.Status.AvailableReplicas)
		}
		member.RollingStrategy.MaxUnavailable.IntVal += unavailableQuotaToAdd
		unavailableQuota -= unavailableQuotaToAdd
	}

	if unavailableQuota <= 0 {
		return memberContexts
	}

	// member.maxUnavailable += FLOOR(unavailableQuota * member.replicas / federationContext.replicas)
	//
	// The rest unavailableQuota will not determinate the number of updated replicas, so
	// we just distribute the rest unavailableQuota to each member cluster by replicas weight
	// to ensure the sum of maxUnavailable is equal to federationContext.maxUnavailable as much as possible.
	for _, member := range memberContexts {
		wightedMaxUnavailable := int32(math.Floor(float64(federationContext.RollingStrategy.MaxUnavailable.IntVal*member.DesiredReplicas) / float64(federationContext.DesiredReplicas)))
		unavailableQuotaToAdd := integer.Int32Max(wightedMaxUnavailable-member.RollingStrategy.MaxUnavailable.IntVal, 0)
		unavailableQuotaToAdd = integer.Int32Min(unavailableQuotaToAdd, unavailableQuota)
		member.RollingStrategy.MaxUnavailable.IntVal += unavailableQuotaToAdd
		unavailableQuota -= unavailableQuotaToAdd
	}

	// handle rest unavailableQuota cased by round down, will not execute
	// more than len(memberContexts) times in the following loops.
	currentIndex, totalIndex := 0, len(memberContexts)
	for totalIndex > 0 && unavailableQuota > 0 {
		memberContexts[currentIndex].RollingStrategy.MaxUnavailable.IntVal += 1
		currentIndex = (currentIndex + 1) % totalIndex
		unavailableQuota--
	}

	return memberContexts
}

func (i *statelessRolloutInterpreterImpl) calculateMaxSurgeForEachCluster(federationContext *rollingContext, memberContexts []*rollingContext) []*rollingContext {
	// updateTargetQuota indicate the number of replicas
	// that can be surged in federated level according to `maxSurge`.
	//
	// initialized by federationContext.maxSurge
	var surgeQuota = federationContext.RollingStrategy.MaxSurge.IntVal

	// member.maxSurge = MIN(surgeQuota, member.current_to_update)
	//
	// This loop ensure not exceed federationContext.maxSurge by controlling the number of replicas to surge,
	// and ensure the surge replicas is effective as much as possible.
	for _, member := range memberContexts {
		if surgeQuota == 0 {
			break
		}
		currentRevisionReplicas := integer.Int32Max(*member.Status.Replicas-*member.Status.UpdatedReplicas, 0)
		currentToUpdateReplicas := integer.Int32Max(currentRevisionReplicas-member.RollingStrategy.Partition.IntVal, 0)
		surgeQuotaToAdd := integer.Int32Min(currentToUpdateReplicas, surgeQuota)
		member.RollingStrategy.MaxSurge.IntVal = surgeQuotaToAdd
		surgeQuota -= surgeQuotaToAdd
	}

	return memberContexts
}

func (i *statelessRolloutInterpreterImpl) correctAndGenerateValidRollingStrategy(memberContexts []*rollingContext) map[string]*configv1alpha1.RollingStrategy {
	for _, member := range memberContexts {
		// Most stateless workloads(e.g., Deployment/CloneSet) require that maxUnavailable and maxSurge
		// must not be both 0, otherwise validation admission webhook will deny the create/update request.
		if member.RollingStrategy.MaxSurge.IntVal == 0 && member.RollingStrategy.MaxUnavailable.IntVal == 0 {
			// Hack: We set maxSurge=1 to ensure the update can be allowed by the validation admission,
			// and ensure NO ONE replicas will be updated due to `member.partition = member.replicas`.
			member.RollingStrategy.MaxSurge.IntVal = 1
			member.RollingStrategy.Partition.IntVal = member.DesiredReplicas
		} else {
			// We hope set partition as much as possible to ensure the newly-updated replicas is under
			// our control, and give a chance to adjust federated partition back.
			target := member.RollingStrategy.Partition.IntVal
			source := *member.Status.Replicas - *member.Status.UpdatedReplicas
			update := member.RollingStrategy.MaxUnavailable.IntVal + member.RollingStrategy.MaxSurge.IntVal
			if quota := source - target - update; quota > 0 {
				member.RollingStrategy.Partition.IntVal += quota
			}
		}
	}

	memberStrategies := make(map[string]*configv1alpha1.RollingStrategy, len(memberContexts))
	for _, member := range memberContexts {
		memberStrategies[member.ClusterName] = member.RollingStrategy
	}
	return memberStrategies
}
