package native

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	hash "github.com/karmada-io/karmada/pkg/util/hasher"
	"github.com/karmada-io/karmada/pkg/util/rollout"
)

type calculateTemplateHashInterpreter func(object *unstructured.Unstructured) (string, error)

func getAllDefaultCalculateTemplateHashInterpreter() map[schema.GroupVersionKind]calculateTemplateHashInterpreter {
	s := make(map[schema.GroupVersionKind]calculateTemplateHashInterpreter)
	s[rollout.CloneSetGroupVersionKind] = cloneSetCalculateTemplateHashInterpreter
	return s
}

func cloneSetCalculateTemplateHashInterpreter(object *unstructured.Unstructured) (string, error) {
	return calculateTemplateHash(object)
}

// calculateTemplateHash calculates the hash of the template of the workload.
func calculateTemplateHash(workload *unstructured.Unstructured) (string, error) {
	template, ok, err := unstructured.NestedFieldNoCopy(workload.Object, "spec", "template")
	if err != nil || !ok {
		return "", fmt.Errorf("failed to nested unstructured template for %s(%s/%s): %v", workload.GetKind(), workload.GetNamespace(), workload.GetName(), err)
	}
	return hash.ComputeHash(template, nil), nil
}
