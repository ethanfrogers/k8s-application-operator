package worker

import (
	"fmt"
	"github.com/ethanfrogers/k8s-application-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"testing"
)

func TestMergeValues(t *testing.T) {
	a := []v1alpha1.Artifact{
		{Values: runtime.RawExtension{Raw: []byte(`{"replicaCount": 1}`)}},
		{Values: runtime.RawExtension{Raw: []byte(`{"replicaCount": 3}`)}},
	}

	m, err := getMergedValues(a)
	fmt.Println(m)
	fmt.Println(err)
	t.Fail()
}
