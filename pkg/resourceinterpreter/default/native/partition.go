package native

import (
	"encoding/json"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// replicaInterpreter is the function that used to parse replica and requirements from object.
type partitionInterpreter func(object *unstructured.Unstructured) (*intstr.IntOrString, error)

func getAllDefaultPartitionInterpreter() map[schema.GroupVersionKind]partitionInterpreter {
	s := make(map[schema.GroupVersionKind]partitionInterpreter)
	cloneSetGVK := schema.GroupVersionKind{
		Group:   "apps.kruise.io",
		Version: "v1alpha1",
		Kind:    "CloneSet",
	}
	s[cloneSetGVK] = cloneSetPartition
	return s
}

func cloneSetPartition(object *unstructured.Unstructured) (*intstr.IntOrString, error) {
	m, found, err := unstructured.NestedFieldCopy(object.Object, "spec", "updateStrategy", "partition")
	if err == nil && found {
		return unmarshalIntStr(m), nil
	}
	return nil, err
}

// unmarshalIntStr return *intstr.IntOrString
func unmarshalIntStr(m interface{}) *intstr.IntOrString {
	field := &intstr.IntOrString{}
	data, _ := json.Marshal(m)
	_ = json.Unmarshal(data, field)
	return field
}
