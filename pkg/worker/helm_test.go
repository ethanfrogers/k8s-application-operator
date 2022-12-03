package worker

import (
	"fmt"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"testing"
)

func TestLabelSelection(t *testing.T) {
	targetLabels := map[string]string{
		"env": "staging",
	}

	labelsSets := map[string]map[string]string{
		"foo1": {
			"env":     "prod",
			"ignored": "true",
		},
		"foo2": {
			"env": "staging",
		},
	}

	requirements := labels.Requirements{}
	for k, v := range targetLabels {
		req, _ := labels.NewRequirement(k, selection.Equals, []string{v})
		requirements = append(requirements, *req)
	}

	var collected []string
	selector := labels.NewSelector().Add(requirements...)
	for k, l := range labelsSets {
		if selector.Matches(labels.Set(l)) {
			collected = append(collected, k)
		}
	}

	fmt.Println(collected)
	t.Fail()

}
