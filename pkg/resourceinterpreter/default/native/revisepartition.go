package native

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type revisePartitionInterpreter func(object *unstructured.Unstructured, partition *intstr.IntOrString) (*unstructured.Unstructured, error)

func getAllDefaultRevisePartitionInterpreter() map[schema.GroupVersionKind]revisePartitionInterpreter {
	s := make(map[schema.GroupVersionKind]revisePartitionInterpreter)
	cloneSetGVK := schema.GroupVersionKind{
		Group:   "apps.kruise.io",
		Version: "v1alpha1",
		Kind:    "CloneSet",
	}
	s[cloneSetGVK] = cloneSetRevisePartition
	return s
}

func cloneSetRevisePartition(object *unstructured.Unstructured, partition *intstr.IntOrString) (*unstructured.Unstructured, error) {
	var err error
	if partition == nil {
		err = unstructured.SetNestedField(object.Object, nil, "spec", "updateStrategy", "partition")
	} else if partition.Type == intstr.Int {
		err = unstructured.SetNestedField(object.Object, partition.IntVal, "spec", "updateStrategy", "partition")
	} else if partition.Type == intstr.String {
		err = unstructured.SetNestedField(object.Object, partition.StrVal, "spec", "updateStrategy", "partition")
	}
	if err != nil {
		return nil, err
	}
	return object, nil
}
