package worker

import (
	"fmt"
	"helm.sh/helm/v3/pkg/cli/values"
	"testing"
)

func TestMergeValues(t *testing.T) {
	s1 := `{"replicaCount": "1"}`
	opts := values.Options{Values: []string{s1}}
	v, err := opts.MergeValues(nil)
	fmt.Println(err)
	fmt.Println(v)
	t.Fail()
}
